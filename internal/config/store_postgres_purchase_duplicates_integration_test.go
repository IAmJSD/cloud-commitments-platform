//go:build integration
// +build integration

package config

// Real-DB tests for purchase_history rows that share a purchase_id (#267).
// Migration 000108 stops new duplicates, but rows saved before it may still
// carry the same purchase_id twice, and deleting them is a pending owner
// decision. The single-row readers and the marketplace claim must treat the
// earliest row (created_at, then id) as the purchase, the same row the first
// save kept, instead of whichever duplicate the planner returns first.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPurchaseHistoryLegacyDuplicates_Postgres(t *testing.T) {
	ctx := context.Background()
	// A migrated database of this test's own on the package's shared
	// container (issue #982); it is dropped when the test ends.
	conn := testhelpers.RequireMigratedDatabase(ctx, t, getTestMigrationsPath()).DB

	store := NewPostgresStore(conn)
	pool := conn.Pool()
	t.Run("LookupReturnsEarliestRow", func(t *testing.T) {
		checkLookupReturnsEarliestDuplicate(ctx, t, store, pool)
	})
	t.Run("ClaimRefusedWhenEarliestRowIsListed", func(t *testing.T) {
		checkClaimHonorsListedEarliestRow(ctx, t, store, pool)
	})
	t.Run("ClaimRefusedWhenAnyDuplicateIsListed", func(t *testing.T) {
		checkClaimHonorsListedTwin(ctx, t, store, pool)
	})
	t.Run("ClaimReportsEarliestRowState", func(t *testing.T) {
		checkClaimReportsEarliestRowState(ctx, t, store, pool)
	})
	t.Run("ClaimReleaseReclaimDoesNotWedge", func(t *testing.T) {
		checkDuplicateClaimDoesNotWedge(ctx, t, store, pool)
	})
	t.Run("ConcurrentClaimWaitsAndResumes", func(t *testing.T) {
		checkConcurrentClaimOverDuplicates(ctx, t, store, pool)
	})
	t.Run("ParallelClaimsShareOneAttempt", func(t *testing.T) {
		checkParallelClaimsOverDuplicates(ctx, t, store, pool)
	})
}

// seedLegacyDuplicate saves one row for id through the store, then adds a
// second row for the same key the way pre-000108 code did. The saved row is
// stamped as the later one, so the earliest row is the second one on disk:
// a lookup that does not order its rows returns the later row first.
// It returns the earliest row's id.
func seedLegacyDuplicate(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool, id string) string {
	t.Helper()
	require.NoError(t, store.SavePurchaseHistory(ctx, &PurchaseHistoryRecord{
		AccountID: "acct", PurchaseID: id, Timestamp: time.Now(), Provider: "aws", Service: "ec2",
		Region: "us-east-1", ResourceType: "t3.micro", Count: 1, Term: 3, Payment: "All Upfront",
	}))
	// Both timestamps predate the partial unique index's cutoff, as legacy rows do.
	_, err := pool.Exec(ctx, `UPDATE purchase_history SET created_at = '2020-01-02T00:00:00Z' WHERE purchase_id = $1`, id)
	require.NoError(t, err)
	var earliest string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO purchase_history (
			account_id, purchase_id, timestamp, provider, service, region,
			resource_type, count, term, payment, upfront_cost, monthly_cost,
			estimated_savings, ramp_step, created_at)
		SELECT account_id, purchase_id, timestamp, provider, service, region,
		       resource_type, count, term, payment, upfront_cost, monthly_cost,
		       estimated_savings, ramp_step, '2020-01-01T00:00:00Z'
		FROM purchase_history WHERE purchase_id = $1
		RETURNING id::text`, id).Scan(&earliest))
	return earliest
}

func setRowListing(ctx context.Context, t *testing.T, pool *pgxpool.Pool, rowID, listingID, state string) {
	t.Helper()
	_, err := pool.Exec(ctx, `UPDATE purchase_history SET listing_id = $2, listing_state = $3 WHERE id = $1::uuid`,
		rowID, listingID, state)
	require.NoError(t, err)
}

func checkLookupReturnsEarliestDuplicate(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	const id = "ri-dup-lookup-267"
	earliest := seedLegacyDuplicate(ctx, t, store, pool, id)
	setRowListing(ctx, t, pool, earliest, "ril-earliest", ListingStateCancelled)

	for range 3 {
		got, err := store.GetPurchaseHistoryByPurchaseID(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "ril-earliest", got.ListingID, "the lookup must return the earliest row, not an arbitrary duplicate")
	}
}

// The earliest row records an active listing and a later duplicate was saved
// without it. Claiming through the duplicate would let the handler create a
// second AWS listing for the same RI.
func checkClaimHonorsListedEarliestRow(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	const id = "ri-dup-active-267"
	earliest := seedLegacyDuplicate(ctx, t, store, pool, id)
	setRowListing(ctx, t, pool, earliest, "ril-live", ListingStateActive)

	claim, err := store.ClaimMarketplaceListingSlot(ctx, id, "tok-1", []byte(claimTestSchedule))
	require.NoError(t, err)
	assert.Nil(t, claim, "a purchase whose earliest row is listed must not be claimable")
	assertNoPendingRows(ctx, t, pool, id)
}

// Same as above with the listing recorded on the later duplicate only.
func checkClaimHonorsListedTwin(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	const id = "ri-dup-twin-active-267"
	earliest := seedLegacyDuplicate(ctx, t, store, pool, id)
	_, err := pool.Exec(ctx, `UPDATE purchase_history SET listing_id = 'ril-live', listing_state = $2
		WHERE purchase_id = $1 AND id <> $3::uuid`, id, ListingStateActive, earliest)
	require.NoError(t, err)

	claim, err := store.ClaimMarketplaceListingSlot(ctx, id, "tok-1", []byte(claimTestSchedule))
	require.NoError(t, err)
	assert.Nil(t, claim, "an active listing on any duplicate must block the claim")
	assertNoPendingRows(ctx, t, pool, id)
}

func assertNoPendingRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	var pending int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM purchase_history WHERE purchase_id = $1 AND listing_state = $2`,
		id, ListingStatePending).Scan(&pending))
	assert.Zero(t, pending, "a refused claim must not leave any row pending")
}

// The claim's prior state and listing id come from the earliest row, and only
// that row is claimed: the handler restores PriorState on release, so a value
// taken from another duplicate, or a release that spans both, would overwrite
// a row's recorded state with the other row's.
func checkClaimReportsEarliestRowState(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	const id = "ri-dup-prior-267"
	earliest := seedLegacyDuplicate(ctx, t, store, pool, id)
	setRowListing(ctx, t, pool, earliest, "ril-old", ListingStateCancelled)
	_, err := pool.Exec(ctx, `UPDATE purchase_history SET listing_id = 'ril-twin', listing_state = $2
		WHERE purchase_id = $1 AND id <> $3::uuid`, id, ListingStateClosed, earliest)
	require.NoError(t, err)

	claim, err := store.ClaimMarketplaceListingSlot(ctx, id, "tok-1", []byte(claimTestSchedule))
	require.NoError(t, err)
	require.NotNil(t, claim)
	assert.Equal(t, ListingStateCancelled, claim.PriorState)
	assert.Equal(t, "ril-old", claim.ListingID)

	require.NoError(t, store.ReleaseMarketplaceListingClaim(ctx, id, claim.ClientToken, claim.PriorState, false))
	var state, listingID string
	require.NoError(t, pool.QueryRow(ctx, `SELECT COALESCE(listing_state, ''), COALESCE(listing_id, '') FROM purchase_history WHERE id = $1::uuid`,
		earliest).Scan(&state, &listingID))
	assert.Equal(t, ListingStateCancelled, state, "release must restore the earliest row's own state")
	assert.Equal(t, "ril-old", listingID)
	require.NoError(t, pool.QueryRow(ctx, `SELECT COALESCE(listing_state, ''), COALESCE(listing_id, '') FROM purchase_history WHERE purchase_id = $1 AND id <> $2::uuid`,
		id, earliest).Scan(&state, &listingID))
	assert.Equal(t, ListingStateClosed, state, "the claim and release must leave the other duplicate as it was")
	assert.Equal(t, "ril-twin", listingID)
}

// The scenario from #267: identical duplicates, a claim that fails on AWS and
// is released, then a retry. Every step must succeed and the listing, once
// recorded, must cover every duplicate.
func checkDuplicateClaimDoesNotWedge(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	const id = "ri-dup-wedge-267"
	seedLegacyDuplicate(ctx, t, store, pool, id)

	claim, err := store.ClaimMarketplaceListingSlot(ctx, id, "tok-1", []byte(claimTestSchedule))
	require.NoError(t, err)
	require.NotNil(t, claim)
	assert.False(t, claim.Resumed)
	require.NoError(t, store.ReleaseMarketplaceListingClaim(ctx, id, claim.ClientToken, claim.PriorState, false))

	claim, err = store.ClaimMarketplaceListingSlot(ctx, id, "tok-2", []byte(claimTestSchedule))
	require.NoError(t, err)
	require.NotNil(t, claim, "a released claim must leave the purchase claimable")
	assert.Equal(t, "tok-2", claim.ClientToken)
	require.NoError(t, store.UpdatePurchaseHistoryListing(ctx, id, "ril-1", ListingStateActive))

	var active int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM purchase_history WHERE purchase_id = $1 AND listing_state = $2 AND listing_id = 'ril-1'`,
		id, ListingStateActive).Scan(&active))
	assert.Equal(t, 2, active, "the recorded listing must cover every duplicate")
	claim, err = store.ClaimMarketplaceListingSlot(ctx, id, "tok-3", []byte(claimTestSchedule))
	require.NoError(t, err)
	assert.Nil(t, claim)
}

// Two claims race over a purchase with duplicates: the second waits for the
// first one's lock on the earliest row and resumes its attempt.
func checkConcurrentClaimOverDuplicates(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	const id = "ri-dup-race-267"
	earliest := seedLegacyDuplicate(ctx, t, store, pool, id)

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = tx.Exec(ctx, `UPDATE purchase_history SET listing_state = 'pending', listing_client_token = 'tok-A',
		listing_price_schedule = '[{"term_months":1,"price":1}]' WHERE id = $1::uuid`, earliest)
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
	}, 10*time.Second, 20*time.Millisecond, "the second claim must wait for the first claim's lock")
	require.NoError(t, tx.Commit(ctx))

	got := <-done
	require.NoError(t, got.err)
	require.NotNil(t, got.claim)
	assert.True(t, got.claim.Resumed)
	assert.Equal(t, "tok-A", got.claim.ClientToken)
	assert.Equal(t, ListingStatePending, got.claim.PriorState)
}

// Many claims at once over a purchase with duplicates: exactly one starts a
// fresh attempt and every other one resumes it with the same token, so the
// handler never sends two different ClientTokens to AWS for one RI.
func checkParallelClaimsOverDuplicates(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	const id = "ri-dup-parallel-267"
	const workers = 8
	seedLegacyDuplicate(ctx, t, store, pool, id)

	claims := make([]*MarketplaceListingClaim, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			claims[i], errs[i] = store.ClaimMarketplaceListingSlot(ctx, id, fmt.Sprintf("tok-%d", i), []byte(claimTestSchedule))
		}()
	}
	close(start)
	wg.Wait()

	fresh := 0
	tokens := map[string]bool{}
	for i := range workers {
		require.NoError(t, errs[i])
		require.NotNil(t, claims[i])
		if !claims[i].Resumed {
			fresh++
		}
		tokens[claims[i].ClientToken] = true
	}
	assert.Equal(t, 1, fresh, "exactly one claim starts the attempt")
	assert.Len(t, tokens, 1, "every claim must carry the same token")
}
