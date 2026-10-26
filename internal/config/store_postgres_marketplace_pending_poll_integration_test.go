//go:build integration
// +build integration

package config

// Real-DB tests for polling a listing recorded as pending (issue #1494). A
// listing AWS gave no status for when it was created is recorded as pending
// with its listing id and no attempt token. The poller must move it to active
// or settle it, but never touch a pending row that is a list attempt in
// flight (it holds a token, and its listing_id is the RI's previous listing),
// and never undo a cancel the cancel handler recorded meanwhile.

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

func TestMarketplacePendingListingPoll_Postgres(t *testing.T) {
	ctx := context.Background()
	conn := testhelpers.RequireMigratedDatabase(ctx, t, getTestMigrationsPath()).DB
	store := NewPostgresStore(conn)
	pool := conn.Pool()

	t.Run("RecordedPendingBecomesActive", func(t *testing.T) { checkPendingBecomesActive(ctx, t, store, pool) })
	t.Run("RecordedPendingSettles", func(t *testing.T) { checkPendingSettles(ctx, t, store, pool) })
	t.Run("MidClaimIsLeftAlone", func(t *testing.T) { checkMidClaimLeftAlone(ctx, t, store, pool) })
	t.Run("CancelCommittingBeforeActivationWins", func(t *testing.T) { checkCancelBeatsActivation(ctx, t, store, pool) })
	t.Run("RelistClaimCommittingBeforePollIsLeftAlone", func(t *testing.T) { checkClaimBeatsPendingPoll(ctx, t, store, pool) })
	t.Run("ParallelCancelAndPollKeepCancel", func(t *testing.T) { checkParallelPendingCancelAndPoll(ctx, t, store, pool) })
}

// seedMidClaim leaves id pending with an attempt token, as a marketplace-list
// request does while it creates a new listing: the row still holds its
// previous, canceled listing previousListingID.
func seedMidClaim(ctx context.Context, t *testing.T, store *PostgresStore, id, previousListingID string) {
	t.Helper()
	seedListing(ctx, t, store, "aws", id, previousListingID, ListingStateCancelled)
	claim, err := store.ClaimMarketplaceListingSlot(ctx, id, "tok-"+id, []byte(`[{"term_months":6,"price":40}]`))
	require.NoError(t, err)
	require.NotNil(t, claim)
}

func readClientToken(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var token string
	require.NoError(t, pool.QueryRow(ctx, `SELECT COALESCE(listing_client_token, '') FROM purchase_history
		WHERE purchase_id = $1`, id).Scan(&token))
	return token
}

func checkPendingBecomesActive(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	at := listedAtFixture()
	seedListing(ctx, t, store, "aws", "pending-active", "ril-pa", ListingStatePending)

	changed, err := store.RecordMarketplaceListingActive(ctx, "pending-active", "ril-pa", at)
	require.NoError(t, err)
	assert.True(t, changed)
	got := readSettlement(ctx, t, pool, "pending-active")
	assert.Equal(t, ListingStateActive, got.state)
	require.NotNil(t, got.listedAt)
	assert.True(t, at.Equal(*got.listedAt))

	changed, err = store.RecordMarketplaceListingActive(ctx, "pending-active", "ril-pa", at)
	require.NoError(t, err)
	assert.False(t, changed, "an unchanged active row is not rewritten")

	// AWS reported no CreateDate: the state still moves, listed_at stays unset.
	seedListing(ctx, t, store, "aws", "pending-active-nodate", "ril-pan", "Pending")
	changed, err = store.RecordMarketplaceListingActive(ctx, "pending-active-nodate", "ril-pan", time.Time{})
	require.NoError(t, err)
	assert.True(t, changed)
	got = readSettlement(ctx, t, pool, "pending-active-nodate")
	assert.Equal(t, ListingStateActive, got.state)
	assert.Nil(t, got.listedAt)

	// A zero CreateDate never clears the listed_at an earlier poll stored.
	changed, err = store.RecordMarketplaceListingActive(ctx, "pending-active", "ril-pa", time.Time{})
	require.NoError(t, err)
	assert.False(t, changed)
	require.NotNil(t, readSettlement(ctx, t, pool, "pending-active").listedAt)

	// A listing id the row does not hold is ignored.
	seedListing(ctx, t, store, "aws", "pending-other", "ril-po", ListingStatePending)
	changed, err = store.RecordMarketplaceListingActive(ctx, "pending-other", "ril-elsewhere", at)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, ListingStatePending, readSettlement(ctx, t, pool, "pending-other").state)
}

func checkPendingSettles(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	seedListing(ctx, t, store, "aws", "pending-settle", "ril-ps1", ListingStatePending)

	applied, err := store.SettleMarketplaceListing(ctx, closedSettlement("pending-settle", "ril-ps1", 880, 120))
	require.NoError(t, err)
	assert.True(t, applied)
	got := readSettlement(ctx, t, pool, "pending-settle")
	assert.Equal(t, ListingStateClosed, got.state)
	require.NotNil(t, got.proceeds)
	assert.InDelta(t, 880, *got.proceeds, 1e-9)

	rows, err := store.ListMarketplaceListingsToPoll(ctx)
	require.NoError(t, err)
	for _, r := range rows {
		assert.NotEqual(t, "pending-settle", r.PurchaseID, "a settled listing is not polled again")
	}
}

func checkMidClaimLeftAlone(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	seedMidClaim(ctx, t, store, "pending-mid-claim", "ril-mc")

	changed, err := store.RecordMarketplaceListingActive(ctx, "pending-mid-claim", "ril-mc", listedAtFixture())
	require.NoError(t, err)
	assert.False(t, changed)
	canceledSettle := closedSettlement("pending-mid-claim", "ril-mc", 0, 0)
	canceledSettle.State = ListingStateCancelled
	applied, err := store.SettleMarketplaceListing(ctx, canceledSettle)
	require.NoError(t, err)
	assert.False(t, applied)

	got := readSettlement(ctx, t, pool, "pending-mid-claim")
	assert.Equal(t, ListingStatePending, got.state)
	assert.Nil(t, got.listedAt)
	assert.Equal(t, "tok-pending-mid-claim", readClientToken(ctx, t, pool, "pending-mid-claim"), "the attempt token is kept")
}

// The poller read the pending listing as active from AWS, then the cancel
// handler commits its cancel while the poller's write waits on the row.
func checkCancelBeatsActivation(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	seedListing(ctx, t, store, "aws", "pending-race-cancel", "ril-prc", ListingStatePending)
	commit := holdCancel(ctx, t, pool, "pending-race-cancel", "ril-prc")

	done := make(chan guardedResult, 1)
	go func() {
		changed, err := store.RecordMarketplaceListingActive(ctx, "pending-race-cancel", "ril-prc", listedAtFixture())
		done <- guardedResult{changed, err}
	}()
	testhelpers.WaitForLockWait(ctx, t, pool, "%listed_at%")
	commit()

	got := testhelpers.Await(t, done)
	require.NoError(t, got.err)
	assert.False(t, got.applied)
	assert.Equal(t, ListingStateCancelled, readSettlement(ctx, t, pool, "pending-race-cancel").state)
}

// The cancel handler cancels the pending listing and a list request claims the
// RI again, both committing while the poller's writes for the old listing wait
// on the row. The row is then pending again with the same listing_id, but it
// is a claim in flight: the poller must not touch it.
func checkClaimBeatsPendingPoll(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	writes := map[string]func(id string) (bool, error){
		"activate": func(id string) (bool, error) {
			return store.RecordMarketplaceListingActive(ctx, id, "ril-pcl", listedAtFixture())
		},
		"settle": func(id string) (bool, error) {
			return store.SettleMarketplaceListing(ctx, closedSettlement(id, "ril-pcl", 100, 10))
		},
	}
	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			id := "pending-race-claim-" + name
			seedListing(ctx, t, store, "aws", id, "ril-pcl", ListingStatePending)
			tx, err := pool.Begin(ctx)
			require.NoError(t, err)
			t.Cleanup(func() { _ = tx.Rollback(ctx) })
			_, err = tx.Exec(ctx, cancelWriteSQL, "ril-pcl", ListingStateCancelled, id)
			require.NoError(t, err)
			_, err = tx.Exec(ctx, `UPDATE purchase_history SET listing_state = $2, listing_client_token = $3
				WHERE purchase_id = $1`, id, ListingStatePending, "tok-"+id)
			require.NoError(t, err)

			done := make(chan guardedResult, 1)
			go func() {
				applied, writeErr := write(id)
				done <- guardedResult{applied, writeErr}
			}()
			testhelpers.WaitForLockWait(ctx, t, pool, "%UPDATE purchase_history%")
			require.NoError(t, tx.Commit(ctx))

			got := testhelpers.Await(t, done)
			require.NoError(t, got.err)
			assert.False(t, got.applied)
			row := readSettlement(ctx, t, pool, id)
			assert.Equal(t, ListingStatePending, row.state)
			assert.Nil(t, row.proceeds)
			assert.Equal(t, "tok-"+id, readClientToken(ctx, t, pool, id))
		})
	}
}

// The cancel handler resolving a pending listing and the poller (which read it
// as active, or as closed) running at once always end with the cancel
// recorded.
func checkParallelPendingCancelAndPoll(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	for round := range 10 {
		id := fmt.Sprintf("pending-parallel-%d", round)
		seedListing(ctx, t, store, "aws", id, "ril-pp", ListingStatePending)
		seen := MarketplaceListingAttempt{ListingID: "ril-pp", State: ListingStatePending}

		start := make(chan struct{})
		var wg sync.WaitGroup
		var resolveErr, activateErr, settleErr error
		var resolved bool
		wg.Go(func() {
			<-start
			resolved, resolveErr = store.ResolveMarketplaceListingAttempt(ctx, id, seen, "ril-pp", ListingStateCancelled)
		})
		wg.Go(func() {
			<-start
			_, activateErr = store.RecordMarketplaceListingActive(ctx, id, "ril-pp", listedAtFixture())
		})
		wg.Go(func() {
			<-start
			_, settleErr = store.SettleMarketplaceListing(ctx, closedSettlement(id, "ril-pp", 100, 10))
		})
		close(start)
		wg.Wait()
		require.NoError(t, resolveErr)
		require.NoError(t, activateErr)
		require.NoError(t, settleErr)
		assert.True(t, resolved, "round %d: the cancel is always recorded", round)
		row := readSettlement(ctx, t, pool, id)
		assert.Equal(t, ListingStateCancelled, row.state, "round %d", round)
		assert.Equal(t, "ril-pp", row.listingID, "round %d", round)
	}
}
