//go:build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/require"
)

// TestMigration_PurchaseHistoryActiveExpiryIndex checks that 000126 creates the
// expiry index GetActivePurchaseHistory relies on (issue #33), that any term
// value still inserts once the index exists, and that the down migration
// removes it.
func TestMigration_PurchaseHistoryActiveExpiryIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool, path := container.DB.Pool(), getMigrationsPath()

	indexDef := func() string {
		var def string
		err := pool.QueryRow(ctx, `
			SELECT COALESCE((SELECT indexdef FROM pg_indexes
			                 WHERE tablename = 'purchase_history'
			                   AND indexname = 'idx_purchase_history_active_expiry'), '')`).Scan(&def)
		require.NoError(t, err)
		return def
	}

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 125))
	require.Empty(t, indexDef(), "before 000126 the index must not exist")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 126))
	def := indexDef()
	require.Contains(t, def, "EXTRACT(epoch FROM (\"timestamp\" AT TIME ZONE 'UTC'::text))")
	require.Contains(t, def, "((term)::numeric * (31536000)::numeric)")
	require.NotContains(t, def, "WHERE", "a partial index's expression statistics are ignored by the planner")

	// numeric arithmetic: an absurd term must not make the INSERT fail on the
	// index expression (int4 or interval arithmetic would overflow here).
	_, err := pool.Exec(ctx, `
		INSERT INTO purchase_history (account_id, purchase_id, timestamp, provider, service, region,
		                              resource_type, term, payment)
		VALUES ('123456789012', 'ri-huge-term', now(), 'aws', 'ec2', 'us-east-1', 'm5.large',
		        2147483647, 'all-upfront')`)
	require.NoError(t, err)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 125))
	require.Empty(t, indexDef(), "down must drop the index")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 126))
	require.NotEmpty(t, indexDef(), "up must re-apply cleanly after down")
}
