//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	migration153Version = 153
	// listingStateCancelled is AWS's ListingStatus spelling, stored verbatim.
	listingStateCancelled = "cancelled" //nolint:misspell // AWS ListingStatus enum literal, not prose
)

// listingSettlementsTableExists reports whether migration 000153's table exists.
func listingSettlementsTableExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT to_regclass('public.marketplace_listing_settlements') IS NOT NULL`).Scan(&exists))
	return exists
}

// seedListingRow inserts a purchase_history row with a listing and, when
// proceeds is non-nil, its settlement. createdAt orders legacy duplicates.
func seedListingRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, purchaseID, listingID, state string,
	createdAt time.Time, listedAt *time.Time, proceeds, fee *float64) string {
	t.Helper()
	var id string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO purchase_history (
			account_id, purchase_id, timestamp, provider, service, region, resource_type,
			term, payment, offering_class, listing_id, listing_state, created_at,
			listed_at, listing_proceeds_received, listing_fee_paid
		) VALUES ('test-account', $1, $2, 'aws', 'ec2', 'us-east-1', 'm5.large',
		          1, 'all-upfront', 'standard', NULLIF($3, ''), NULLIF($4, ''), $2, $5, $6, $7)
		RETURNING id::text`,
		purchaseID, createdAt, listingID, state, listedAt, proceeds, fee).Scan(&id))
	return id
}

type migratedListingSettlement struct {
	historyID, purchaseID, status string
	listedAt                      *time.Time
	proceeds, fee                 *float64
	settledAt                     *time.Time
}

func readMigratedListingSettlements(ctx context.Context, t *testing.T, pool *pgxpool.Pool) map[string]migratedListingSettlement {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT listing_id, purchase_history_id::text, purchase_id, status, listed_at,
		       proceeds_received::float8, fee_paid::float8, settled_at
		FROM marketplace_listing_settlements`)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]migratedListingSettlement{}
	for rows.Next() {
		var listingID string
		var r migratedListingSettlement
		require.NoError(t, rows.Scan(&listingID, &r.historyID, &r.purchaseID, &r.status, &r.listedAt,
			&r.proceeds, &r.fee, &r.settledAt))
		out[listingID] = r
	}
	require.NoError(t, rows.Err())
	return out
}

func amount(v float64) *float64 { return &v }

// TestMigration153_MarketplaceListingSettlements locks down migration 000153
// (issue #1494): it creates the per-listing settlement table, backfills one row
// per listing purchase_history records a settlement for (the earliest row of
// legacy duplicates), keeps a listing the RI moves on from via its trigger,
// and its rollback drops the table and trigger while purchase_history keeps
// its settlement columns and values.
//
// Fail-before: at the previous version the table does not exist and a relist
// keeps nothing of the previous listing.
func TestMigration153_MarketplaceListingSettlements(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	container := testhelpers.RequirePostgresContainer(ctx, t)
	defer container.Cleanup(ctx)
	pool := container.DB.Pool()
	migrationsPath := getMigrationsPath()

	prev := previousMigrationVersion(t, migration153Version)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, prev))
	require.False(t, listingSettlementsTableExists(ctx, t, pool))

	listedAt := time.Date(2026, 9, 1, 12, 30, 0, 0, time.UTC)
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	settledID := seedListingRow(ctx, t, pool, "ri-153-settled", "ril-153-s", "closed", old, &listedAt, amount(880), amount(120))
	// Legacy duplicates of one RI hold the same listing: one backfilled row,
	// from the earliest.
	dupFirst := seedListingRow(ctx, t, pool, "ri-153-dup", "ril-153-d", listingStateCancelled, old, &listedAt, amount(88), amount(12))
	seedListingRow(ctx, t, pool, "ri-153-dup", "ril-153-d", listingStateCancelled, old.Add(time.Hour), &listedAt, amount(88), amount(12))
	// Not backfilled: unsettled, never listed.
	seedListingRow(ctx, t, pool, "ri-153-unsettled", "ril-153-u", listingStateCancelled, old, &listedAt, nil, nil)
	seedListingRow(ctx, t, pool, "ri-153-unlisted", "", "", old, nil, nil, nil)

	// Before 000153 a relist leaves no trace of the previous listing.
	_, err := pool.Exec(ctx, `UPDATE purchase_history SET listing_id = 'ril-153-u2', listing_state = 'active'
		WHERE purchase_id = 'ri-153-unsettled'`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE purchase_history SET listing_id = 'ril-153-u', listing_state = 'cancelled'
		WHERE purchase_id = 'ri-153-unsettled'`)
	require.NoError(t, err)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, migration153Version))
	require.True(t, listingSettlementsTableExists(ctx, t, pool))
	got := readMigratedListingSettlements(ctx, t, pool)
	require.Len(t, got, 2, "one row per settled listing")
	s := got["ril-153-s"]
	assert.Equal(t, settledID, s.historyID)
	assert.Equal(t, "ri-153-settled", s.purchaseID)
	assert.Equal(t, "closed", s.status)
	require.NotNil(t, s.listedAt)
	assert.True(t, listedAt.Equal(*s.listedAt))
	require.NotNil(t, s.proceeds)
	require.NotNil(t, s.fee)
	assert.InDelta(t, 880, *s.proceeds, 1e-9)
	assert.InDelta(t, 120, *s.fee, 1e-9)
	assert.NotNil(t, s.settledAt)
	d := got["ril-153-d"]
	assert.Equal(t, dupFirst, d.historyID, "legacy duplicates backfill from the earliest row")
	assert.InDelta(t, 88, *d.proceeds, 1e-9)

	// The trigger keeps the listing the RI moves on from, as it was recorded.
	_, err = pool.Exec(ctx, `UPDATE purchase_history SET listing_id = 'ril-153-u2', listing_state = 'active'
		WHERE purchase_id = 'ri-153-unsettled'`)
	require.NoError(t, err)
	got = readMigratedListingSettlements(ctx, t, pool)
	require.Contains(t, got, "ril-153-u")
	u := got["ril-153-u"]
	assert.Equal(t, listingStateCancelled, u.status)
	require.NotNil(t, u.listedAt)
	assert.True(t, listedAt.Equal(*u.listedAt))
	assert.Nil(t, u.proceeds)
	assert.Nil(t, u.settledAt)

	// Proceeds and fee are stored together, to the cent.
	_, err = pool.Exec(ctx, `UPDATE marketplace_listing_settlements SET proceeds_received = 1 WHERE listing_id = 'ril-153-u'`)
	require.Error(t, err, "proceeds without a fee is rejected")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, prev))
	require.False(t, listingSettlementsTableExists(ctx, t, pool))
	var triggers, functions int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM pg_trigger
		WHERE tgname = 'purchase_history_keep_superseded_listing'`).Scan(&triggers))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM pg_proc
		WHERE proname = 'keep_superseded_marketplace_listing'`).Scan(&functions))
	assert.Zero(t, triggers)
	assert.Zero(t, functions)
	var phProceeds *float64
	require.NoError(t, pool.QueryRow(ctx, `SELECT listing_proceeds_received::float8 FROM purchase_history
		WHERE purchase_id = 'ri-153-settled'`).Scan(&phProceeds))
	require.NotNil(t, phProceeds, "the rollback keeps purchase_history's settlement")
	assert.InDelta(t, 880, *phProceeds, 1e-9)
	_, err = pool.Exec(ctx, `UPDATE purchase_history SET listing_id = 'ril-153-u3' WHERE purchase_id = 'ri-153-unsettled'`)
	require.NoError(t, err, "a relist after the rollback needs no trigger")

	// Re-applying after a rollback backfills again.
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, migration153Version))
	assert.Len(t, readMigratedListingSettlements(ctx, t, pool), 2)
}
