//go:build integration
// +build integration

package config

// Real-DB tests for relisting an RI whose previous listing was already settled
// (issue #27). AWS has no price edit, so repricing is cancel and relist. The
// new listing can end before the poller ever sees it active, so the list path
// itself must drop the previous listing's listed_at and settlement, or the
// settle guard (proceeds IS NULL) refuses the new listing's sale forever.

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarketplaceListingRelist_Postgres(t *testing.T) {
	ctx := context.Background()
	conn := testhelpers.RequireMigratedDatabase(ctx, t, getTestMigrationsPath()).DB
	store := NewPostgresStore(conn)
	pool := conn.Pool()

	t.Run("RelistClosesBeforeFirstPoll", func(t *testing.T) { checkRelistThenClose(ctx, t, store, pool) })
	t.Run("RelistCanceledBeforeFirstPoll", func(t *testing.T) { checkRelistThenCancel(ctx, t, store, pool) })
	t.Run("ResolvedAttemptDropsOldSettlement", func(t *testing.T) { checkResolvedRelist(ctx, t, store, pool) })
	t.Run("SameListingKeepsSettlement", func(t *testing.T) { checkSameListingKeepsSettlement(ctx, t, store, pool) })
}

// settleCanceledAtZero runs the first listing's lifecycle: listed, seen active
// by the poller, canceled by the user, settled at 0/0 by the next poll. It
// then claims the slot for a relist, as the list handler does.
func settleCanceledAtZero(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool, id, l1 string) {
	t.Helper()
	seedListing(ctx, t, store, "aws", id, l1, ListingStateActive)
	changed, err := store.RecordMarketplaceListingActive(ctx, id, l1, listedAtFixture())
	require.NoError(t, err)
	require.True(t, changed)
	require.NoError(t, store.UpdatePurchaseHistoryListing(ctx, id, l1, ListingStateCancelled))
	canceled := closedSettlement(id, l1, 0, 0)
	canceled.State = ListingStateCancelled
	applied, err := store.SettleMarketplaceListing(ctx, canceled)
	require.NoError(t, err)
	require.True(t, applied)
	require.NotNil(t, readSettlement(ctx, t, pool, id).proceeds)

	claim, err := store.ClaimMarketplaceListingSlot(ctx, id, "tok-"+id, []byte(`[{"term_months":6,"price":40}]`))
	require.NoError(t, err)
	require.NotNil(t, claim, "a canceled row can be listed again")
}

// assertSettlementCleared checks that the row holds listingID in state and
// none of the previous listing's settlement.
func assertSettlementCleared(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id, listingID, state string) {
	t.Helper()
	got := readSettlement(ctx, t, pool, id)
	assert.Equal(t, listingID, got.listingID)
	assert.Equal(t, state, got.state)
	assert.Nil(t, got.listedAt, "the previous listing's listed_at is dropped")
	assert.Nil(t, got.proceeds, "the previous listing's proceeds are dropped")
	assert.Nil(t, got.fee, "the previous listing's fee is dropped")
}

func checkRelistThenClose(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	const id = "relist-close"
	settleCanceledAtZero(ctx, t, store, pool, id, "ril-l1")
	require.NoError(t, store.UpdatePurchaseHistoryListing(ctx, id, "ril-l2", ListingStateActive))
	assertSettlementCleared(ctx, t, pool, id, "ril-l2", ListingStateActive)

	// L2 sold out before the next hourly poll: the poll settles it directly.
	l2 := closedSettlement(id, "ril-l2", 1760, 240)
	l2Created := listedAtFixture().Add(48 * time.Hour)
	l2.ListedAt = &l2Created
	applied, err := store.SettleMarketplaceListing(ctx, l2)
	require.NoError(t, err)
	assert.True(t, applied, "the relisted listing's sale is recorded")

	got := readSettlement(ctx, t, pool, id)
	assert.Equal(t, ListingStateClosed, got.state)
	require.NotNil(t, got.listedAt)
	assert.True(t, l2Created.Equal(*got.listedAt))
	require.NotNil(t, got.proceeds)
	require.NotNil(t, got.fee)
	assert.InDelta(t, 1760, *got.proceeds, 1e-9)
	assert.InDelta(t, 240, *got.fee, 1e-9)
	assertNotPolled(ctx, t, store, id)
}

func checkRelistThenCancel(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	const id = "relist-cancel"
	settleCanceledAtZero(ctx, t, store, pool, id, "ril-c1")
	require.NoError(t, store.UpdatePurchaseHistoryListing(ctx, id, "ril-c2", ListingStateActive))
	// The user cancels L2 within the hour, before any poll saw it.
	require.NoError(t, store.UpdatePurchaseHistoryListing(ctx, id, "ril-c2", ListingStateCancelled))
	assertSettlementCleared(ctx, t, pool, id, "ril-c2", ListingStateCancelled)

	// L1's 0/0 is not shown as L2's settlement, and L2's own cancel
	// settlement still applies when a poll reports it.
	l2 := closedSettlement(id, "ril-c2", 88, 12)
	l2.State = ListingStateCancelled
	applied, err := store.SettleMarketplaceListing(ctx, l2)
	require.NoError(t, err)
	assert.True(t, applied)
	got := readSettlement(ctx, t, pool, id)
	assert.Equal(t, ListingStateCancelled, got.state)
	require.NotNil(t, got.proceeds)
	assert.InDelta(t, 88, *got.proceeds, 1e-9)
	assertNotPolled(ctx, t, store, id)
}

// checkResolvedRelist covers the other list-path writer: the cancel endpoint
// recovering a pending relist attempt whose AWS listing already ended.
func checkResolvedRelist(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	const id = "relist-resolve"
	settleCanceledAtZero(ctx, t, store, pool, id, "ril-r1")
	seen, err := store.GetMarketplaceListingAttempt(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, seen)
	applied, err := store.ResolveMarketplaceListingAttempt(ctx, id, *seen, "ril-r2", ListingStateCancelled)
	require.NoError(t, err)
	require.True(t, applied)
	assertSettlementCleared(ctx, t, pool, id, "ril-r2", ListingStateCancelled)
}

// checkSameListingKeepsSettlement pins that only a new listing id clears:
// re-recording the listing the row already holds (a cancel retry) keeps it.
func checkSameListingKeepsSettlement(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	const id = "relist-same"
	at := listedAtFixture()
	seedListing(ctx, t, store, "aws", id, "ril-s1", ListingStateCancelled)
	setSettlement(ctx, t, pool, id, &at, settleAmount(88), settleAmount(12))
	require.NoError(t, store.UpdatePurchaseHistoryListing(ctx, id, "ril-s1", ListingStateCancelled))
	got := readSettlement(ctx, t, pool, id)
	require.NotNil(t, got.listedAt)
	assert.True(t, at.Equal(*got.listedAt))
	require.NotNil(t, got.proceeds)
	assert.InDelta(t, 88, *got.proceeds, 1e-9)
}

func assertNotPolled(ctx context.Context, t *testing.T, store *PostgresStore, id string) {
	t.Helper()
	rows, err := store.ListMarketplaceListingsToPoll(ctx)
	require.NoError(t, err)
	for _, r := range rows {
		assert.NotEqual(t, id, r.PurchaseID, "a settled listing is not polled again")
	}
}
