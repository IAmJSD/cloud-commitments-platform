//go:build integration
// +build integration

package config

// Real-DB tests for per-listing RI Marketplace settlements (issue #1494, gap
// 2). purchase_history only describes an RI's latest listing, so a listing
// that sold partially and was canceled, then relisted before a poll saw the
// sale, used to lose its proceeds. Migration 000153 keeps every listing in
// marketplace_listing_settlements; these tests check that both listings'
// proceeds are kept, that polls are idempotent and race-safe, and that a sale
// is never counted twice.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type listingSettlementRow struct {
	purchaseID, status string
	listedAt           *time.Time
	proceeds, fee      *float64
	settledAt          *time.Time
}

// readListingSettlement returns the marketplace_listing_settlements row of
// listingID, or nil when there is none.
func readListingSettlement(ctx context.Context, t *testing.T, pool *pgxpool.Pool, listingID string) *listingSettlementRow {
	t.Helper()
	var r listingSettlementRow
	err := pool.QueryRow(ctx, `
		SELECT purchase_id, status, listed_at, proceeds_received::float8, fee_paid::float8, settled_at
		FROM marketplace_listing_settlements WHERE listing_id = $1`, listingID).
		Scan(&r.purchaseID, &r.status, &r.listedAt, &r.proceeds, &r.fee, &r.settledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	require.NoError(t, err)
	return &r
}

// requireListingProceeds asserts the recorded proceeds of listingID's
// settlement row.
func requireListingProceeds(ctx context.Context, t *testing.T, pool *pgxpool.Pool, listingID string, want float64) {
	t.Helper()
	row := readListingSettlement(ctx, t, pool, listingID)
	require.NotNil(t, row, "listing %s has a settlement row", listingID)
	require.NotNil(t, row.proceeds, "listing %s has proceeds", listingID)
	assert.InDelta(t, want, *row.proceeds, 1e-9, "listing %s proceeds", listingID)
}

// requireRowProceeds asserts the proceeds on purchase id's purchase_history row.
func requireRowProceeds(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id string, want float64) {
	t.Helper()
	got := readSettlement(ctx, t, pool, id).proceeds
	require.NotNil(t, got, "purchase %s has proceeds", id)
	assert.InDelta(t, want, *got, 1e-9, "purchase %s proceeds", id)
}

// relist runs the marketplace-list handler's store calls for a new listing:
// claim the slot, then record the listing AWS created.
func relist(ctx context.Context, t *testing.T, store *PostgresStore, id, newListingID string) {
	t.Helper()
	claim, err := store.ClaimMarketplaceListingSlot(ctx, id, "tok-"+newListingID, []byte(`[{"term_months":6,"price":40}]`))
	require.NoError(t, err)
	require.NotNil(t, claim)
	require.NoError(t, store.UpdatePurchaseHistoryListing(ctx, id, newListingID, ListingStateActive))
}

// seedCanceledSeenListing records listingID on a new RI as a listing the poller
// saw active (listed_at set) and the cancel handler then canceled: the state a
// partial sale is left in until the next poll.
func seedCanceledSeenListing(ctx context.Context, t *testing.T, store *PostgresStore, id, listingID string) {
	t.Helper()
	seedListing(ctx, t, store, "aws", id, listingID, ListingStateActive)
	_, err := store.RecordMarketplaceListingActive(ctx, id, listingID, listedAtFixture())
	require.NoError(t, err)
	require.NoError(t, store.UpdatePurchaseHistoryListing(ctx, id, listingID, ListingStateCancelled))
}

func canceledSettlement(id, listingID string, proceeds, fee float64) MarketplaceListingSettlement {
	st := closedSettlement(id, listingID, proceeds, fee)
	st.State = ListingStateCancelled
	return st
}

// pollListings returns the poll rows of purchase id, keyed by listing id.
func pollListings(ctx context.Context, t *testing.T, store *PostgresStore, id string) map[string]PurchaseHistoryRecord {
	t.Helper()
	rows, err := store.ListMarketplaceListingsToPoll(ctx)
	require.NoError(t, err)
	out := map[string]PurchaseHistoryRecord{}
	for _, r := range rows {
		if r.PurchaseID == id {
			_, dup := out[r.ListingID]
			require.False(t, dup, "listing %s returned twice", r.ListingID)
			out[r.ListingID] = r
		}
	}
	return out
}

// requireTotal asserts the RI's proceeds totals across listings.
func requireTotal(ctx context.Context, t *testing.T, store *PostgresStore, id string, proceeds, fee float64, settled, unsettled int) {
	t.Helper()
	totals, err := store.GetMarketplaceListingProceedsTotals(ctx, []string{id})
	require.NoError(t, err)
	require.Len(t, totals, 1)
	got := totals[0]
	assert.Equal(t, id, got.PurchaseID)
	assert.Equal(t, "USD", got.Currency)
	assert.InDelta(t, proceeds, got.ProceedsReceived, 1e-9, "proceeds total")
	assert.InDelta(t, fee, got.FeePaid, 1e-9, "fee total")
	assert.Equal(t, settled, got.SettledListings, "settled listings")
	assert.Equal(t, unsettled, got.UnsettledListings, "unsettled listings")
}

func TestMarketplaceListingSettlements_Postgres(t *testing.T) {
	ctx := context.Background()
	conn := testhelpers.RequireMigratedDatabase(ctx, t, getTestMigrationsPath()).DB
	store := NewPostgresStore(conn)
	pool := conn.Pool()

	t.Run("RelistAfterPartialSaleKeepsBothListings", func(t *testing.T) { checkRelistKeepsBothListings(ctx, t, store, pool) })
	t.Run("RepollIsIdempotent", func(t *testing.T) { checkRepollIdempotent(ctx, t, store, pool) })
	t.Run("UnpricedSupersededListingIsPolledUntilPriced", func(t *testing.T) { checkUnpricedSuperseded(ctx, t, store, pool) })
	t.Run("ReleasedClaimCountsListingOnce", func(t *testing.T) { checkReleasedClaimCountsOnce(ctx, t, store, pool) })
	t.Run("SettleWaitingOnClaimLeavesListingForLater", func(t *testing.T) { checkSettleWaitingOnClaim(ctx, t, store, pool) })
	t.Run("ResumedClaimKeepsListing", func(t *testing.T) { checkResumedClaimKeepsListing(ctx, t, store, pool) })
	t.Run("ConcurrentPollsSettleSupersededOnce", func(t *testing.T) { checkConcurrentSupersededSettles(ctx, t, store, pool) })
	t.Run("ConcurrentRelistAndPollKeepProceedsOnce", func(t *testing.T) { checkConcurrentRelistAndPoll(ctx, t, store, pool) })
	t.Run("TotalsOfUnknownPurchases", func(t *testing.T) { checkTotalsOfUnknownPurchases(ctx, t, store) })
}

// The reported bug: listing A sells partially, is canceled, and the RI is
// relisted as B before a poll sees A's sale. Both listings' proceeds must be
// recorded, and the RI's total is their sum.
func checkRelistKeepsBothListings(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	const id = "mls-relist"
	seedCanceledSeenListing(ctx, t, store, id, "ril-mls-a")
	relist(ctx, t, store, id, "ril-mls-b")

	due := pollListings(ctx, t, store, id)
	require.Contains(t, due, "ril-mls-a", "the listing the RI was relisted from is still polled")
	require.Contains(t, due, "ril-mls-b")
	a := due["ril-mls-a"]
	assert.Equal(t, ListingStateCancelled, a.ListingState, "the kept listing has its own last state, not the claim's")
	assert.Equal(t, "acct", a.AccountID)
	assert.Equal(t, "us-east-1", a.Region)
	assert.Equal(t, "aws", a.Provider)

	applied, err := store.SettleMarketplaceListing(ctx, canceledSettlement(id, "ril-mls-a", 88, 12))
	require.NoError(t, err)
	assert.True(t, applied, "the superseded listing's partial sale is recorded")
	applied, err = store.SettleMarketplaceListing(ctx, closedSettlement(id, "ril-mls-b", 176, 24))
	require.NoError(t, err)
	assert.True(t, applied)

	rowA := readListingSettlement(ctx, t, pool, "ril-mls-a")
	require.NotNil(t, rowA)
	assert.Equal(t, id, rowA.purchaseID)
	assert.Equal(t, ListingStateCancelled, rowA.status)
	require.NotNil(t, rowA.listedAt)
	assert.True(t, listedAtFixture().Equal(*rowA.listedAt))
	require.NotNil(t, rowA.proceeds)
	require.NotNil(t, rowA.fee)
	assert.InDelta(t, 88, *rowA.proceeds, 1e-9)
	assert.InDelta(t, 12, *rowA.fee, 1e-9)
	assert.NotNil(t, rowA.settledAt)
	rowB := readListingSettlement(ctx, t, pool, "ril-mls-b")
	require.NotNil(t, rowB)
	assert.Equal(t, ListingStateClosed, rowB.status)
	requireListingProceeds(ctx, t, pool, "ril-mls-b", 176)

	// purchase_history keeps the latest listing's settlement.
	ph := readSettlement(ctx, t, pool, id)
	assert.Equal(t, "ril-mls-b", ph.listingID)
	assert.Equal(t, ListingStateClosed, ph.state)
	requireRowProceeds(ctx, t, pool, id, 176)

	requireTotal(ctx, t, store, id, 264, 36, 2, 0)
	assert.Empty(t, pollListings(ctx, t, store, id), "settled listings are not polled again")
}

func checkRepollIdempotent(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	const id = "mls-repoll"
	seedCanceledSeenListing(ctx, t, store, id, "ril-mrp-a")
	relist(ctx, t, store, id, "ril-mrp-b")
	applied, err := store.SettleMarketplaceListing(ctx, canceledSettlement(id, "ril-mrp-a", 88, 12))
	require.NoError(t, err)
	require.True(t, applied)
	applied, err = store.SettleMarketplaceListing(ctx, closedSettlement(id, "ril-mrp-b", 50, 7))
	require.NoError(t, err)
	require.True(t, applied)

	for range 3 {
		applied, err = store.SettleMarketplaceListing(ctx, canceledSettlement(id, "ril-mrp-a", 1000, 1))
		require.NoError(t, err)
		assert.False(t, applied, "a superseded listing is settled once")
		applied, err = store.SettleMarketplaceListing(ctx, closedSettlement(id, "ril-mrp-b", 1000, 1))
		require.NoError(t, err)
		assert.False(t, applied, "the current listing is settled once")
	}
	requireListingProceeds(ctx, t, pool, "ril-mrp-a", 88)
	requireListingProceeds(ctx, t, pool, "ril-mrp-b", 50)
	requireRowProceeds(ctx, t, pool, id, 50)
	requireTotal(ctx, t, store, id, 138, 19, 2, 0)
}

// A superseded listing whose sale price is not known yet records its state
// and stays due; a later poll that prices it records it once.
func checkUnpricedSuperseded(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	const id = "mls-unpriced"
	seedCanceledSeenListing(ctx, t, store, id, "ril-mup-a")
	relist(ctx, t, store, id, "ril-mup-b")

	at := listedAtFixture()
	unpriced := MarketplaceListingSettlement{PurchaseID: id, ListingID: "ril-mup-a", State: ListingStateCancelled, ListedAt: &at}
	applied, err := store.SettleMarketplaceListing(ctx, unpriced)
	require.NoError(t, err)
	assert.True(t, applied)
	row := readListingSettlement(ctx, t, pool, "ril-mup-a")
	require.NotNil(t, row)
	assert.Nil(t, row.proceeds)
	assert.Nil(t, row.settledAt)
	assert.Contains(t, pollListings(ctx, t, store, id), "ril-mup-a", "an unpriced listing is read again")
	requireTotal(ctx, t, store, id, 0, 0, 0, 1)

	applied, err = store.SettleMarketplaceListing(ctx, canceledSettlement(id, "ril-mup-a", 44, 6))
	require.NoError(t, err)
	assert.True(t, applied)
	assert.NotContains(t, pollListings(ctx, t, store, id), "ril-mup-a")
	requireTotal(ctx, t, store, id, 44, 6, 1, 0)
}

// A failed relist releases its claim, so the RI's row holds listing A again
// while A is also kept in the settlement table. A is polled once per run and
// its proceeds count once.
func checkReleasedClaimCountsOnce(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	const id = "mls-release"
	seedCanceledSeenListing(ctx, t, store, id, "ril-mrl-a")
	claim, err := store.ClaimMarketplaceListingSlot(ctx, id, "tok-mrl", []byte(`[{"term_months":6,"price":40}]`))
	require.NoError(t, err)
	require.NotNil(t, claim)
	require.NotNil(t, readListingSettlement(ctx, t, pool, "ril-mrl-a"), "the claim keeps the listing it replaces")
	requireMidClaimLeftAlone(ctx, t, store, pool, id, "ril-mrl-a")
	require.NoError(t, store.ReleaseMarketplaceListingClaim(ctx, id, "tok-mrl", claim.PriorState, false))

	due := pollListings(ctx, t, store, id)
	require.Len(t, due, 1)
	require.Contains(t, due, "ril-mrl-a")

	applied, err := store.SettleMarketplaceListing(ctx, canceledSettlement(id, "ril-mrl-a", 88, 12))
	require.NoError(t, err)
	assert.True(t, applied)
	requireRowProceeds(ctx, t, pool, id, 88)
	requireListingProceeds(ctx, t, pool, "ril-mrl-a", 88)
	requireTotal(ctx, t, store, id, 88, 12, 1, 0)

	applied, err = store.SettleMarketplaceListing(ctx, canceledSettlement(id, "ril-mrl-a", 88, 12))
	require.NoError(t, err)
	assert.False(t, applied)
	requireTotal(ctx, t, store, id, 88, 12, 1, 0)
}

// requireMidClaimLeftAlone asserts that while purchase id's row holds listingID
// as a list claim in flight, the listing is not polled and a poll's settle of
// it writes neither the row nor the listing's settlement row (#1508).
func requireMidClaimLeftAlone(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool, id, listingID string) {
	t.Helper()
	assert.NotContains(t, pollListings(ctx, t, store, id), listingID, "a listing held by a claim in flight is not polled")
	applied, err := store.SettleMarketplaceListing(ctx, canceledSettlement(id, listingID, 88, 12))
	require.NoError(t, err)
	assert.False(t, applied, "a claim in flight is left alone")
	assert.Nil(t, readSettlement(ctx, t, pool, id).proceeds)
	if row := readListingSettlement(ctx, t, pool, listingID); row != nil {
		assert.Nil(t, row.proceeds, "the kept listing is not settled while the claim is in flight")
	}
}

// A poll of listing A that queues behind a relist claim committing on the
// same RI re-checks the row, finds the claim, and writes nothing. A is not
// lost: once the claim resolves to the new listing, A is polled from the row
// the claim kept for it and its sale is recorded once.
func checkSettleWaitingOnClaim(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	const id = "mls-held-claim"
	seedCanceledSeenListing(ctx, t, store, id, "ril-mhc-a")
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	_, err = tx.Exec(ctx, `UPDATE purchase_history SET listing_state = 'pending',
		listing_client_token = 'tok-mhc' WHERE purchase_id = $1`, id)
	require.NoError(t, err)

	done := make(chan guardedResult, 1)
	go func() {
		applied, settleErr := store.SettleMarketplaceListing(ctx, canceledSettlement(id, "ril-mhc-a", 88, 12))
		done <- guardedResult{applied, settleErr}
	}()
	testhelpers.WaitForLockWait(ctx, t, pool, "%listing_proceeds_received%")
	require.NoError(t, tx.Commit(ctx))

	got := testhelpers.Await(t, done)
	require.NoError(t, got.err)
	assert.False(t, got.applied, "a claim in flight is left alone")
	ph := readSettlement(ctx, t, pool, id)
	assert.Equal(t, ListingStatePending, ph.state)
	assert.Nil(t, ph.proceeds)
	kept := readListingSettlement(ctx, t, pool, "ril-mhc-a")
	require.NotNil(t, kept, "the claim keeps the listing")
	assert.Nil(t, kept.proceeds)
	requireMidClaimLeftAlone(ctx, t, store, pool, id, "ril-mhc-a")

	require.NoError(t, store.UpdatePurchaseHistoryListing(ctx, id, "ril-mhc-b", ListingStateActive))
	require.Contains(t, pollListings(ctx, t, store, id), "ril-mhc-a", "the listing is polled once the claim resolves")
	applied, err := store.SettleMarketplaceListing(ctx, canceledSettlement(id, "ril-mhc-a", 88, 12))
	require.NoError(t, err)
	assert.True(t, applied)
	requireListingProceeds(ctx, t, pool, "ril-mhc-a", 88)
	requireTotal(ctx, t, store, id, 88, 12, 1, 0)
}

// A list attempt released with its token kept (an ambiguous AWS outcome) puts
// the row back in listing A's own state; if A's first claim left no row in
// the settlement table (it predates migration 000153), the retry's claim keeps
// A, so a relist does not lose it.
func checkResumedClaimKeepsListing(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	const id = "mls-resume"
	seedCanceledSeenListing(ctx, t, store, id, "ril-mrs-a")
	// The state ReleaseMarketplaceListingClaim(keepAttempt=true) leaves: A's
	// own state with the attempt's token and price schedule kept. Written with
	// triggers off, as by a claim and release that ran before migration 000153.
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	_, err = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `UPDATE purchase_history SET listing_client_token = 'tok-mrs',
		listing_price_schedule = '[{"term_months":6,"price":40}]'::jsonb WHERE purchase_id = $1`, id)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	require.Nil(t, readListingSettlement(ctx, t, pool, "ril-mrs-a"), "fixture: no row kept yet")

	claim, err := store.ClaimMarketplaceListingSlot(ctx, id, "tok-ignored", []byte(`[{"term_months":6,"price":40}]`))
	require.NoError(t, err)
	require.NotNil(t, claim)
	require.True(t, claim.Resumed)
	kept := readListingSettlement(ctx, t, pool, "ril-mrs-a")
	require.NotNil(t, kept, "the resumed claim keeps the listing")
	assert.Equal(t, ListingStateCancelled, kept.status)
	requireMidClaimLeftAlone(ctx, t, store, pool, id, "ril-mrs-a")

	require.NoError(t, store.UpdatePurchaseHistoryListing(ctx, id, "ril-mrs-b", ListingStateActive))
	applied, err := store.SettleMarketplaceListing(ctx, canceledSettlement(id, "ril-mrs-a", 88, 12))
	require.NoError(t, err)
	assert.True(t, applied)
	requireTotal(ctx, t, store, id, 88, 12, 1, 0)
}

// Many pollers settling the same superseded listing at once record it exactly
// once, with the winner's values.
func checkConcurrentSupersededSettles(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	const pollers = 8
	for round := range 5 {
		id := fmt.Sprintf("mls-parallel-%d", round)
		listingA := fmt.Sprintf("ril-mpp-a-%d", round)
		seedCanceledSeenListing(ctx, t, store, id, listingA)
		relist(ctx, t, store, id, fmt.Sprintf("ril-mpp-b-%d", round))

		start := make(chan struct{})
		results := make([]guardedResult, pollers)
		var wg sync.WaitGroup
		for i := range pollers {
			wg.Go(func() {
				<-start
				applied, err := store.SettleMarketplaceListing(ctx, canceledSettlement(id, listingA, float64(100+i), float64(i)))
				results[i] = guardedResult{applied, err}
			})
		}
		close(start)
		wg.Wait()

		winners, winner := 0, 0
		for i, r := range results {
			require.NoError(t, r.err)
			if r.applied {
				winners++
				winner = i
			}
		}
		require.Equal(t, 1, winners, "round %d: exactly one settle applies", round)
		requireListingProceeds(ctx, t, pool, listingA, float64(100+winner))
		requireTotal(ctx, t, store, id, float64(100+winner), float64(winner), 1, 0)
	}
}

// A relist and two polls of the listing it replaces run at once and end with
// that listing's sale recorded exactly once, and no deadlock. In even rounds
// all three start together; in odd rounds the polls start once the relist's
// claim has committed (so the claim has kept the listing) and race the write
// of the new listing id. A poll that runs while the claim is in flight writes
// nothing (#1508); the listing must then still be due, and the next poll
// records it. Both ways the listing's row can be created are hit: by a poll
// that settles it while the RI still holds it, and by the claim.
func checkConcurrentRelistAndPoll(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	keptByClaim, deferred := 0, 0
	for round := range 20 {
		id := fmt.Sprintf("mls-race-%d", round)
		listingA := fmt.Sprintf("ril-mrr-a-%d", round)
		seedCanceledSeenListing(ctx, t, store, id, listingA)

		start, claimed := make(chan struct{}), make(chan struct{})
		var wg sync.WaitGroup
		var relistErr error
		wg.Go(func() {
			<-start
			claim, err := store.ClaimMarketplaceListingSlot(ctx, id, "tok-"+id, []byte(`[{"term_months":6,"price":40}]`))
			close(claimed)
			if err != nil || claim == nil {
				relistErr = fmt.Errorf("claim: %v, %w", claim, err)
				return
			}
			relistErr = store.UpdatePurchaseHistoryListing(ctx, id, fmt.Sprintf("ril-mrr-b-%d", round), ListingStateActive)
		})
		results := make([]guardedResult, 2)
		for i := range results {
			wg.Go(func() {
				<-start
				if round%2 == 1 {
					<-claimed
				}
				applied, err := store.SettleMarketplaceListing(ctx, canceledSettlement(id, listingA, 88, 12))
				results[i] = guardedResult{applied, err}
			})
		}
		close(start)
		wg.Wait()
		require.NoError(t, relistErr, "round %d", round)
		require.NoError(t, results[0].err, "round %d", round)
		require.NoError(t, results[1].err, "round %d", round)
		require.False(t, results[0].applied && results[1].applied, "round %d: the sale is recorded at most once", round)

		// The next poll run: due exactly when no poll recorded the sale.
		_, due := pollListings(ctx, t, store, id)[listingA]
		recorded := results[0].applied || results[1].applied
		assert.Equal(t, !recorded, due, "round %d: an unrecorded sale is still due", round)
		applied, err := store.SettleMarketplaceListing(ctx, canceledSettlement(id, listingA, 88, 12))
		require.NoError(t, err)
		assert.Equal(t, !recorded, applied, "round %d: the next poll records it only if no earlier poll did", round)
		if !recorded {
			deferred++
		}

		row := readListingSettlement(ctx, t, pool, listingA)
		require.NotNil(t, row, "round %d", round)
		require.NotNil(t, row.proceeds, "round %d", round)
		assert.Equal(t, ListingStateCancelled, row.status, "round %d", round)
		requireTotal(ctx, t, store, id, 88, 12, 1, 0)
		var claimFirst bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT settled_at > created_at FROM marketplace_listing_settlements
			WHERE listing_id = $1`, listingA).Scan(&claimFirst))
		if claimFirst {
			keptByClaim++
		}
	}
	t.Logf("listing row created by the relist claim in %d of 20 rounds; sale deferred past the claim in %d", keptByClaim, deferred)
	assert.GreaterOrEqual(t, keptByClaim, 10, "every odd round settles a listing the claim kept")
}

func checkTotalsOfUnknownPurchases(ctx context.Context, t *testing.T, store *PostgresStore) {
	totals, err := store.GetMarketplaceListingProceedsTotals(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, totals)
	totals, err = store.GetMarketplaceListingProceedsTotals(ctx, []string{"mls-never-listed"})
	require.NoError(t, err)
	assert.Empty(t, totals)
}
