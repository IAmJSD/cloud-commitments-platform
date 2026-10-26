//go:build integration
// +build integration

package config

// Real-DB tests for the marketplace listing claim (issue #525). The claim,
// release and record statements decide whether a retry may resume an attempt,
// so their predicates are checked against Postgres rather than a mock.

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const claimTestSchedule = `[{"term_months":36,"price":100}]`

func TestMarketplaceListingClaim_PostgresSemantics(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequireMigratedDatabase(ctx, t, getTestMigrationsPath())

	store := NewPostgresStore(container.DB)
	checkMarketplaceListingClaim(ctx, t, store)
	checkConcurrentMarketplaceClaim(ctx, t, store, container.DB.Pool())
	checkMarketplaceReleaseIsFenced(ctx, t, store)
}

func seedClaimRow(ctx context.Context, t *testing.T, store *PostgresStore, id string) {
	t.Helper()
	require.NoError(t, store.SavePurchaseHistory(ctx, &PurchaseHistoryRecord{
		AccountID: "acct", PurchaseID: id, Timestamp: time.Now(), Provider: "aws", Service: "ec2",
		Region: "us-east-1", ResourceType: "t3.micro", Count: 1, Term: 3, Payment: "All Upfront",
	}))
}

// A claim that races another one waits for its row lock and must come back as
// a resumed claim carrying the first claim's token and schedule. The first
// claim is held in an open transaction, as it is while its statement commits.
func checkConcurrentMarketplaceClaim(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	t.Helper()
	const id = "ri-race-525"
	seedClaimRow(ctx, t, store, id)

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `UPDATE purchase_history SET listing_state = 'pending', listing_client_token = 'tok-A',
		listing_price_schedule = '[{"term_months":1,"price":1}]' WHERE purchase_id = $1`, id)
	require.NoError(t, err)

	type result struct {
		claim *MarketplaceListingClaim
		err   error
	}
	done := make(chan result, 1)
	go func() {
		claim, claimErr := store.ClaimMarketplaceListingSlot(ctx, id, "tok-B", []byte(`[{"term_months":2,"price":2}]`))
		done <- result{claim, claimErr}
	}()

	require.Eventually(t, func() bool {
		var waiting int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%listing_client_token%'`).Scan(&waiting))
		return waiting > 0
	}, 10*time.Second, 20*time.Millisecond, "the second claim must be blocked on the first claim's row lock")
	require.NoError(t, tx.Commit(ctx))

	got := <-done
	require.NoError(t, got.err)
	require.NotNil(t, got.claim)
	assert.True(t, got.claim.Resumed, "the second claim is a resumed one")
	assert.Equal(t, "tok-A", got.claim.ClientToken)
	assert.JSONEq(t, `[{"term_months":1,"price":1}]`, string(got.claim.PriceSchedule))
	assert.Equal(t, ListingStatePending, got.claim.PriorState)
}

// A request only releases or drops the attempt it owns.
func checkMarketplaceReleaseIsFenced(ctx context.Context, t *testing.T, store *PostgresStore) {
	t.Helper()
	const id = "ri-fence-525"
	seedClaimRow(ctx, t, store, id)
	claim, err := store.ClaimMarketplaceListingSlot(ctx, id, "tok-owner", []byte(claimTestSchedule))
	require.NoError(t, err)
	require.NotNil(t, claim)

	require.NoError(t, store.ReleaseMarketplaceListingClaim(ctx, id, "tok-stale", "", false))
	require.NoError(t, store.ReleaseMarketplaceListingClaim(ctx, id, "tok-stale", "", true))
	got, err := store.GetPurchaseHistoryByPurchaseID(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, ListingStatePending, got.ListingState, "a stale release must not touch the row")
	resumed, err := store.ClaimMarketplaceListingSlot(ctx, id, "tok-next", []byte(claimTestSchedule))
	require.NoError(t, err)
	require.NotNil(t, resumed)
	assert.Equal(t, "tok-owner", resumed.ClientToken, "a stale drop must not erase the owner's token")
}

func checkMarketplaceListingClaim(ctx context.Context, t *testing.T, store *PostgresStore) {
	t.Helper()
	const id = "ri-claim-525"
	require.NoError(t, store.SavePurchaseHistory(ctx, &PurchaseHistoryRecord{
		AccountID: "acct", PurchaseID: id, Timestamp: time.Now(), Provider: "aws", Service: "ec2",
		Region: "us-east-1", ResourceType: "t3.micro", Count: 1, Term: 3, Payment: "All Upfront",
	}))
	row := func() (listingID, state string) {
		got, err := store.GetPurchaseHistoryByPurchaseID(ctx, id)
		require.NoError(t, err)
		return got.ListingID, got.ListingState
	}

	// A fresh row is claimed and the token and schedule are stored with it.
	claim, err := store.ClaimMarketplaceListingSlot(ctx, id, "tok-1", []byte(claimTestSchedule))
	require.NoError(t, err)
	require.NotNil(t, claim)
	assert.False(t, claim.Resumed)
	assert.Equal(t, "tok-1", claim.ClientToken)
	assert.JSONEq(t, claimTestSchedule, string(claim.PriceSchedule))
	assert.Empty(t, claim.PriorState)
	_, state := row()
	assert.Equal(t, ListingStatePending, state)

	// A pending row holding a token can be claimed again: the stored token and
	// schedule win over the new ones, and the claim says it resumed.
	claim, err = store.ClaimMarketplaceListingSlot(ctx, id, "tok-2", []byte(`[{"term_months":35,"price":1}]`))
	require.NoError(t, err)
	require.NotNil(t, claim)
	assert.True(t, claim.Resumed)
	assert.Equal(t, "tok-1", claim.ClientToken)
	assert.JSONEq(t, claimTestSchedule, string(claim.PriceSchedule))
	assert.Equal(t, ListingStatePending, claim.PriorState)

	// Release keeping the attempt: the state returns, the token stays.
	require.NoError(t, store.ReleaseMarketplaceListingClaim(ctx, id, "tok-1", "", true))
	_, state = row()
	assert.Empty(t, state)
	claim, err = store.ClaimMarketplaceListingSlot(ctx, id, "tok-3", []byte(claimTestSchedule))
	require.NoError(t, err)
	require.NotNil(t, claim)
	assert.True(t, claim.Resumed)
	assert.Equal(t, "tok-1", claim.ClientToken)

	// Release dropping the attempt: the next claim starts with its own token.
	require.NoError(t, store.ReleaseMarketplaceListingClaim(ctx, id, "tok-1", "", false))
	claim, err = store.ClaimMarketplaceListingSlot(ctx, id, "tok-4", []byte(claimTestSchedule))
	require.NoError(t, err)
	require.NotNil(t, claim)
	assert.False(t, claim.Resumed)
	assert.Equal(t, "tok-4", claim.ClientToken)

	// Recording a listing resolves the attempt and clears the token.
	require.NoError(t, store.UpdatePurchaseHistoryListing(ctx, id, "ril-1", ListingStateActive))
	claim, err = store.ClaimMarketplaceListingSlot(ctx, id, "tok-5", []byte(claimTestSchedule))
	require.NoError(t, err)
	assert.Nil(t, claim, "an active listing is never claimable")

	// After a cancel the claim sees the recorded listing and a fresh attempt;
	// release only touches a pending row, never a recorded listing.
	require.NoError(t, store.UpdatePurchaseHistoryListing(ctx, id, "ril-1", ListingStateCancelled))
	claim, err = store.ClaimMarketplaceListingSlot(ctx, id, "tok-6", []byte(claimTestSchedule))
	require.NoError(t, err)
	require.NotNil(t, claim)
	assert.False(t, claim.Resumed)
	assert.Equal(t, "ril-1", claim.ListingID)
	assert.Equal(t, ListingStateCancelled, claim.PriorState)
	require.NoError(t, store.UpdatePurchaseHistoryListing(ctx, id, "ril-2", ListingStateActive))
	require.NoError(t, store.ReleaseMarketplaceListingClaim(ctx, id, "tok-6", ListingStateCancelled, false))
	listingID, state := row()
	assert.Equal(t, "ril-2", listingID)
	assert.Equal(t, ListingStateActive, state, "release must not overwrite a recorded listing")

	// Pending with no token (a legacy stuck row) is not claimable; an absent row is not either.
	require.NoError(t, store.UpdatePurchaseHistoryListing(ctx, id, "ril-2", ListingStatePending))
	claim, err = store.ClaimMarketplaceListingSlot(ctx, id, "tok-7", []byte(claimTestSchedule))
	require.NoError(t, err)
	assert.Nil(t, claim)
	claim, err = store.ClaimMarketplaceListingSlot(ctx, "no-such-ri", "tok-8", []byte(claimTestSchedule))
	require.NoError(t, err)
	assert.Nil(t, claim)
}
