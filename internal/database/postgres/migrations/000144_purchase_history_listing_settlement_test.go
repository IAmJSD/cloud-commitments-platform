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

const migration144Version = 144

// purchaseHistoryColumnExists reports whether purchase_history has the column.
func purchaseHistoryColumnExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, column string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_name = 'purchase_history' AND column_name = $1)`, column).Scan(&exists))
	return exists
}

// TestMigration144_ListingSettlementColumns locks down migration 000144
// (issue #27): it adds the listing settlement columns the status poller
// writes, leaves existing rows' settlement unset, and its rollback drops only
// its own columns, not listing_price_schedule (owned by 000105).
//
// Fail-before: at the previous version the columns do not exist, so the
// existence assertions and the settlement write fail.
func TestMigration144_ListingSettlementColumns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	container := testhelpers.RequirePostgresContainer(ctx, t)
	defer container.Cleanup(ctx)
	pool := container.DB.Pool()
	migrationsPath := getMigrationsPath()
	settlementColumns := []string{"listed_at", "listing_proceeds_received", "listing_fee_paid"}

	prev := previousMigrationVersion(t, migration144Version)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, prev))
	for _, c := range settlementColumns {
		assert.False(t, purchaseHistoryColumnExists(ctx, t, pool, c), "before 000144: %s", c)
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO purchase_history (
			account_id, purchase_id, timestamp, provider, service, region,
			resource_type, term, payment, offering_class, listing_id, listing_state
		) VALUES ('test-account', 'ri-144', $1, 'aws', 'ec2', 'us-east-1',
		          'm5.large', 1, 'all-upfront', 'standard', 'ril-144', 'active')`, time.Now())
	require.NoError(t, err)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, migration144Version))
	var listedAt *time.Time
	var proceeds, fee *float64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT listed_at, listing_proceeds_received::float8, listing_fee_paid::float8
		FROM purchase_history WHERE purchase_id = 'ri-144'`).Scan(&listedAt, &proceeds, &fee))
	assert.Nil(t, listedAt, "an existing listing is not backfilled")
	assert.Nil(t, proceeds)
	assert.Nil(t, fee)

	// Money is stored to the cent.
	_, err = pool.Exec(ctx, `
		UPDATE purchase_history
		SET listed_at = now(), listing_proceeds_received = 880.004, listing_fee_paid = 119.996
		WHERE purchase_id = 'ri-144'`)
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT listing_proceeds_received::float8, listing_fee_paid::float8
		FROM purchase_history WHERE purchase_id = 'ri-144'`).Scan(&proceeds, &fee))
	require.NotNil(t, proceeds)
	require.NotNil(t, fee)
	assert.InDelta(t, 880.00, *proceeds, 1e-9)
	assert.InDelta(t, 120.00, *fee, 1e-9)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, prev))
	for _, c := range settlementColumns {
		assert.False(t, purchaseHistoryColumnExists(ctx, t, pool, c), "after rollback: %s", c)
	}
	assert.True(t, purchaseHistoryColumnExists(ctx, t, pool, "listing_price_schedule"),
		"the rollback must not drop 000105's listing_price_schedule")

	// Re-applying after a rollback works.
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, migration144Version))
	for _, c := range settlementColumns {
		assert.True(t, purchaseHistoryColumnExists(ctx, t, pool, c), "after re-apply: %s", c)
	}
}
