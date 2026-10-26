//go:build integration
// +build integration

package config

// Real-DB tests for the marketplace status poller's store statements
// (issue #27). Whether a poll result is recorded is decided by the UPDATEs'
// guards, so they are checked against Postgres, including the races with a
// second poller and with the cancel handler.

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cancelWriteSQL is UpdatePurchaseHistoryListing's write for a cancel of the
// listing the row already holds (so it keeps the settlement), run inside a
// held transaction to stand in for a cancel that is committing.
const cancelWriteSQL = `UPDATE purchase_history SET listing_id = $1, listing_state = $2,
	listing_client_token = NULL, listing_price_schedule = NULL WHERE purchase_id = $3`

type settlementRow struct {
	listingID, state string
	listedAt         *time.Time
	proceeds, fee    *float64
}

func TestMarketplaceListingSettlement_Postgres(t *testing.T) {
	ctx := context.Background()
	// A migrated database of this test's own on the package's shared
	// container (issue #982); it is dropped when the test ends.
	conn := testhelpers.RequireMigratedDatabase(ctx, t, getTestMigrationsPath()).DB
	store := NewPostgresStore(conn)
	pool := conn.Pool()

	t.Run("PollSelection", func(t *testing.T) { checkPollSelection(ctx, t, store, pool) })
	t.Run("ActiveRefreshIsGuarded", func(t *testing.T) { checkActiveRefresh(ctx, t, store, pool) })
	t.Run("SettleIsGuarded", func(t *testing.T) { checkSettleGuards(ctx, t, store, pool) })
	t.Run("SettleRejectsBadInput", func(t *testing.T) { checkSettleValidation(ctx, t, store) })
	t.Run("CancelCommittingBeforeActiveRefreshWins", func(t *testing.T) { checkCancelBeatsActiveRefresh(ctx, t, store, pool) })
	t.Run("CancelCommittingBeforeCloseWins", func(t *testing.T) { checkCancelBeatsClose(ctx, t, store, pool) })
	t.Run("SecondSettleWaitsAndSkips", func(t *testing.T) { checkSecondSettleSkips(ctx, t, store, pool) })
	t.Run("ParallelSettlesRecordOnce", func(t *testing.T) { checkParallelSettles(ctx, t, store, pool) })
	t.Run("ParallelCancelAndPollKeepCancel", func(t *testing.T) { checkParallelCancelAndPoll(ctx, t, store, pool) })
}

// seedListing saves a purchase and records listingID on it in state.
func seedListing(ctx context.Context, t *testing.T, store *PostgresStore, provider, id, listingID, state string) {
	t.Helper()
	require.NoError(t, store.SavePurchaseHistory(ctx, &PurchaseHistoryRecord{
		AccountID: "acct", PurchaseID: id, Timestamp: time.Now(), Provider: provider, Service: "ec2",
		Region: "us-east-1", ResourceType: "t3.micro", Count: 2, Term: 1, Payment: "All Upfront",
	}))
	if listingID != "" || state != "" {
		require.NoError(t, store.UpdatePurchaseHistoryListing(ctx, id, listingID, state))
	}
}

func setSettlement(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id string, listedAt *time.Time, proceeds, fee *float64) {
	t.Helper()
	_, err := pool.Exec(ctx, `UPDATE purchase_history SET listed_at = $2, listing_proceeds_received = $3,
		listing_fee_paid = $4 WHERE purchase_id = $1`, id, listedAt, proceeds, fee)
	require.NoError(t, err)
}

func readSettlement(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id string) settlementRow {
	t.Helper()
	var r settlementRow
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COALESCE(listing_id, ''), COALESCE(listing_state, ''), listed_at,
		       listing_proceeds_received::float8, listing_fee_paid::float8
		FROM purchase_history WHERE purchase_id = $1`, id).
		Scan(&r.listingID, &r.state, &r.listedAt, &r.proceeds, &r.fee))
	return r
}

func settleAmount(v float64) *float64 { return &v }

// listedAtFixture is a microsecond-precision instant so it round-trips through
// timestamptz unchanged.
func listedAtFixture() time.Time {
	return time.Date(2026, 9, 1, 12, 30, 0, 0, time.UTC)
}

func closedSettlement(id, listingID string, proceeds, fee float64) MarketplaceListingSettlement {
	at := listedAtFixture()
	return MarketplaceListingSettlement{PurchaseID: id, ListingID: listingID, State: ListingStateClosed,
		ListedAt: &at, ProceedsReceived: settleAmount(proceeds), FeePaid: settleAmount(fee)}
}

func checkPollSelection(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	seen := listedAtFixture()
	seedListing(ctx, t, store, "aws", "poll-active", "ril-active", ListingStateActive)
	seedListing(ctx, t, store, "aws", "poll-active-upper", "ril-active-upper", "Active")
	seedListing(ctx, t, store, "aws", "poll-cancel-unsettled", "ril-cu", ListingStateCancelled)
	setSettlement(ctx, t, pool, "poll-cancel-unsettled", &seen, nil, nil)
	seedListing(ctx, t, store, "aws", "poll-closed-unpriced", "ril-cl", ListingStateClosed)
	setSettlement(ctx, t, pool, "poll-closed-unpriced", &seen, nil, nil)
	// Excluded: settled, never seen by the poller, mid-claim, not listed, not AWS.
	seedListing(ctx, t, store, "aws", "poll-closed-settled", "ril-cs", ListingStateClosed)
	setSettlement(ctx, t, pool, "poll-closed-settled", &seen, settleAmount(88), settleAmount(12))
	seedListing(ctx, t, store, "aws", "poll-cancel-legacy", "ril-legacy", ListingStateCancelled)
	seedListing(ctx, t, store, "aws", "poll-pending", "ril-pending", ListingStatePending)
	seedListing(ctx, t, store, "aws", "poll-unlisted", "", "")
	seedListing(ctx, t, store, "azure", "poll-azure", "ril-azure", ListingStateActive)

	rows, err := store.ListMarketplaceListingsToPoll(ctx)
	require.NoError(t, err)
	got := map[string]PurchaseHistoryRecord{}
	for _, r := range rows {
		if strings.HasPrefix(r.PurchaseID, "poll-") {
			got[r.PurchaseID] = r
		}
	}
	assert.ElementsMatch(t, []string{"poll-active", "poll-active-upper", "poll-cancel-unsettled", "poll-closed-unpriced"}, slices.Collect(maps.Keys(got)))
	active := got["poll-active"]
	assert.Equal(t, "ril-active", active.ListingID)
	assert.Equal(t, ListingStateActive, active.ListingState)
	assert.Equal(t, "us-east-1", active.Region)
	assert.Equal(t, "acct", active.AccountID)
	assert.Equal(t, "aws", active.Provider)
}

func checkActiveRefresh(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	at := listedAtFixture()
	seedListing(ctx, t, store, "aws", "refresh-1", "ril-r1", ListingStateActive)

	changed, err := store.RecordMarketplaceListingActive(ctx, "refresh-1", "ril-r1", at)
	require.NoError(t, err)
	assert.True(t, changed)
	got := readSettlement(ctx, t, pool, "refresh-1")
	require.NotNil(t, got.listedAt)
	assert.True(t, at.Equal(*got.listedAt))
	assert.Equal(t, ListingStateActive, got.state, "the refresh never writes the state")

	changed, err = store.RecordMarketplaceListingActive(ctx, "refresh-1", "ril-r1", at)
	require.NoError(t, err)
	assert.False(t, changed, "an unchanged row is not rewritten")

	changed, err = store.RecordMarketplaceListingActive(ctx, "refresh-1", "ril-other", at.Add(time.Hour))
	require.NoError(t, err)
	assert.False(t, changed, "a listing the row no longer holds is ignored")

	// A relisted RI still carries the previous listing's settlement: cleared.
	setSettlement(ctx, t, pool, "refresh-1", &at, settleAmount(10), settleAmount(1))
	changed, err = store.RecordMarketplaceListingActive(ctx, "refresh-1", "ril-r1", at)
	require.NoError(t, err)
	assert.True(t, changed)
	got = readSettlement(ctx, t, pool, "refresh-1")
	assert.Nil(t, got.proceeds)
	assert.Nil(t, got.fee)

	// A recorded cancel is never revived or touched.
	seedListing(ctx, t, store, "aws", "refresh-canceled", "ril-rc", ListingStateCancelled)
	changed, err = store.RecordMarketplaceListingActive(ctx, "refresh-canceled", "ril-rc", at)
	require.NoError(t, err)
	assert.False(t, changed)
	got = readSettlement(ctx, t, pool, "refresh-canceled")
	assert.Equal(t, ListingStateCancelled, got.state)
	assert.Nil(t, got.listedAt)
}

func checkSettleGuards(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	at := listedAtFixture()

	// active -> closed records the proceeds once.
	seedListing(ctx, t, store, "aws", "settle-1", "ril-s1", ListingStateActive)
	applied, err := store.SettleMarketplaceListing(ctx, closedSettlement("settle-1", "ril-s1", 1760, 240))
	require.NoError(t, err)
	assert.True(t, applied)
	got := readSettlement(ctx, t, pool, "settle-1")
	assert.Equal(t, ListingStateClosed, got.state)
	require.NotNil(t, got.listedAt)
	assert.True(t, at.Equal(*got.listedAt))
	require.NotNil(t, got.proceeds)
	require.NotNil(t, got.fee)
	assert.InDelta(t, 1760, *got.proceeds, 1e-9)
	assert.InDelta(t, 240, *got.fee, 1e-9)

	applied, err = store.SettleMarketplaceListing(ctx, closedSettlement("settle-1", "ril-s1", 1, 1))
	require.NoError(t, err)
	assert.False(t, applied, "proceeds are recorded once")
	assert.InDelta(t, 1760, *readSettlement(ctx, t, pool, "settle-1").proceeds, 1e-9)

	// A cancel recorded first is never turned into a sale...
	seedListing(ctx, t, store, "aws", "settle-canceled", "ril-sc", ListingStateCancelled)
	applied, err = store.SettleMarketplaceListing(ctx, closedSettlement("settle-canceled", "ril-sc", 100, 10))
	require.NoError(t, err)
	assert.False(t, applied)
	got = readSettlement(ctx, t, pool, "settle-canceled")
	assert.Equal(t, ListingStateCancelled, got.state)
	assert.Nil(t, got.proceeds)
	// ...but its partial-sale proceeds are still recorded once AWS reports the cancel.
	cancelSettle := closedSettlement("settle-canceled", "ril-sc", 88, 12)
	cancelSettle.State = "Cancelled" //nolint:misspell // AWS ListingStatus enum literal
	applied, err = store.SettleMarketplaceListing(ctx, cancelSettle)
	require.NoError(t, err)
	assert.True(t, applied)
	got = readSettlement(ctx, t, pool, "settle-canceled")
	assert.Equal(t, ListingStateCancelled, got.state, "the state is stored lowercase")
	require.NotNil(t, got.proceeds)
	assert.InDelta(t, 88, *got.proceeds, 1e-9)

	// A row relisted under another listing, or mid-claim, is left alone.
	seedListing(ctx, t, store, "aws", "settle-relisted", "ril-new", ListingStateActive)
	applied, err = store.SettleMarketplaceListing(ctx, closedSettlement("settle-relisted", "ril-old", 5, 1))
	require.NoError(t, err)
	assert.False(t, applied)
	seedListing(ctx, t, store, "aws", "settle-pending", "ril-sp", ListingStatePending)
	applied, err = store.SettleMarketplaceListing(ctx, closedSettlement("settle-pending", "ril-sp", 5, 1))
	require.NoError(t, err)
	assert.False(t, applied)
	assert.Equal(t, ListingStatePending, readSettlement(ctx, t, pool, "settle-pending").state)

	// An unpriced close records the state and leaves the row for a later poll.
	seedListing(ctx, t, store, "aws", "settle-unpriced", "ril-su", ListingStateActive)
	unpriced := MarketplaceListingSettlement{PurchaseID: "settle-unpriced", ListingID: "ril-su", State: ListingStateClosed, ListedAt: &at}
	applied, err = store.SettleMarketplaceListing(ctx, unpriced)
	require.NoError(t, err)
	assert.True(t, applied)
	got = readSettlement(ctx, t, pool, "settle-unpriced")
	assert.Equal(t, ListingStateClosed, got.state)
	assert.Nil(t, got.proceeds)
	applied, err = store.SettleMarketplaceListing(ctx, closedSettlement("settle-unpriced", "ril-su", 50, 7))
	require.NoError(t, err)
	assert.True(t, applied, "a later poll that can price the sale records it")
	assert.InDelta(t, 50, *readSettlement(ctx, t, pool, "settle-unpriced").proceeds, 1e-9)
}

func checkSettleValidation(ctx context.Context, t *testing.T, store *PostgresStore) {
	seedListing(ctx, t, store, "aws", "settle-bad", "ril-bad", ListingStateActive)
	bad := closedSettlement("settle-bad", "ril-bad", 1, 1)
	bad.State = ListingStateActive
	_, err := store.SettleMarketplaceListing(ctx, bad)
	require.Error(t, err)
	half := closedSettlement("settle-bad", "ril-bad", 1, 1)
	half.FeePaid = nil
	_, err = store.SettleMarketplaceListing(ctx, half)
	require.Error(t, err)
}

// holdCancel opens a transaction that has written the cancel handler's update
// but not committed it, so the poller's write queues behind its row lock.
func holdCancel(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id, listingID string) func() {
	t.Helper()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	_, err = tx.Exec(ctx, cancelWriteSQL, listingID, ListingStateCancelled, id)
	require.NoError(t, err)
	return func() { require.NoError(t, tx.Commit(ctx)) }
}

type guardedResult struct {
	applied bool
	err     error
}

// The poller read the listing as active from AWS, then the cancel handler
// commits its cancel while the poller's write waits on the row: the write must
// re-check the row and leave the cancel in place.
func checkCancelBeatsActiveRefresh(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	seedListing(ctx, t, store, "aws", "race-refresh", "ril-rr", ListingStateActive)
	commit := holdCancel(ctx, t, pool, "race-refresh", "ril-rr")

	done := make(chan guardedResult, 1)
	go func() {
		changed, err := store.RecordMarketplaceListingActive(ctx, "race-refresh", "ril-rr", listedAtFixture())
		done <- guardedResult{changed, err}
	}()
	testhelpers.WaitForLockWait(ctx, t, pool, "%listed_at%")
	commit()

	got := testhelpers.Await(t, done)
	require.NoError(t, got.err)
	assert.False(t, got.applied)
	assert.Equal(t, ListingStateCancelled, readSettlement(ctx, t, pool, "race-refresh").state)
}

// A close read from AWS that races a committing cancel must not overwrite it.
func checkCancelBeatsClose(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	seedListing(ctx, t, store, "aws", "race-close", "ril-rcl", ListingStateActive)
	commit := holdCancel(ctx, t, pool, "race-close", "ril-rcl")

	done := make(chan guardedResult, 1)
	go func() {
		applied, err := store.SettleMarketplaceListing(ctx, closedSettlement("race-close", "ril-rcl", 100, 10))
		done <- guardedResult{applied, err}
	}()
	testhelpers.WaitForLockWait(ctx, t, pool, "%listing_proceeds_received%")
	commit()

	got := testhelpers.Await(t, done)
	require.NoError(t, got.err)
	assert.False(t, got.applied)
	row := readSettlement(ctx, t, pool, "race-close")
	assert.Equal(t, ListingStateCancelled, row.state)
	assert.Nil(t, row.proceeds)
}

// A second poller's settle that queues behind the first one's must see the
// recorded proceeds and skip.
func checkSecondSettleSkips(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	seedListing(ctx, t, store, "aws", "race-settle", "ril-rs", ListingStateActive)
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	_, err = tx.Exec(ctx, `UPDATE purchase_history SET listing_state = 'closed', listing_proceeds_received = 1760,
		listing_fee_paid = 240 WHERE purchase_id = 'race-settle'`)
	require.NoError(t, err)

	done := make(chan guardedResult, 1)
	go func() {
		applied, settleErr := store.SettleMarketplaceListing(ctx, closedSettlement("race-settle", "ril-rs", 1, 1))
		done <- guardedResult{applied, settleErr}
	}()
	testhelpers.WaitForLockWait(ctx, t, pool, "%listing_proceeds_received%")
	require.NoError(t, tx.Commit(ctx))

	got := testhelpers.Await(t, done)
	require.NoError(t, got.err)
	assert.False(t, got.applied)
	assert.InDelta(t, 1760, *readSettlement(ctx, t, pool, "race-settle").proceeds, 1e-9)
}

// Many pollers settling the same close at once record it exactly once, with
// the winner's values.
func checkParallelSettles(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	const pollers = 8
	for round := range 5 {
		id := fmt.Sprintf("parallel-settle-%d", round)
		seedListing(ctx, t, store, "aws", id, "ril-ps", ListingStateActive)

		start := make(chan struct{})
		results := make([]guardedResult, pollers)
		var wg sync.WaitGroup
		for i := range pollers {
			wg.Go(func() {
				<-start
				applied, err := store.SettleMarketplaceListing(ctx, closedSettlement(id, "ril-ps", float64(100+i), float64(i)))
				results[i] = guardedResult{applied, err}
			})
		}
		close(start)
		wg.Wait()

		winners := 0
		var winner int
		for i, r := range results {
			require.NoError(t, r.err)
			if r.applied {
				winners++
				winner = i
			}
		}
		require.Equal(t, 1, winners, "round %d: exactly one settle applies", round)
		row := readSettlement(ctx, t, pool, id)
		assert.InDelta(t, float64(100+winner), *row.proceeds, 1e-9)
		assert.InDelta(t, float64(winner), *row.fee, 1e-9)
	}
}

// The cancel handler and the poller (which read the listing as active, or as
// closed) running at once always end with the cancel recorded.
func checkParallelCancelAndPoll(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	for round := range 10 {
		id := fmt.Sprintf("parallel-cancel-%d", round)
		seedListing(ctx, t, store, "aws", id, "ril-pc", ListingStateActive)

		start := make(chan struct{})
		var wg sync.WaitGroup
		var cancelErr, refreshErr, settleErr error
		wg.Go(func() {
			<-start
			cancelErr = store.UpdatePurchaseHistoryListing(ctx, id, "ril-pc", ListingStateCancelled)
		})
		wg.Go(func() {
			<-start
			_, refreshErr = store.RecordMarketplaceListingActive(ctx, id, "ril-pc", listedAtFixture())
		})
		wg.Go(func() {
			<-start
			_, settleErr = store.SettleMarketplaceListing(ctx, closedSettlement(id, "ril-pc", 100, 10))
		})
		close(start)
		wg.Wait()
		require.NoError(t, cancelErr)
		require.NoError(t, refreshErr)
		require.NoError(t, settleErr)
		// The cancel handler's write is unconditional, so a close that landed
		// first is overwritten by it; what must never happen is the cancel
		// being undone.
		assert.Equal(t, ListingStateCancelled, readSettlement(ctx, t, pool, id).state, "round %d", round)
	}
}
