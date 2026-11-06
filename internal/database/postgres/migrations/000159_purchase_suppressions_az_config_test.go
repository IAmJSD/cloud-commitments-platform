//go:build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	exec159 = "00000000-0000-0000-0000-000000159a01"

	insertRDSSuppression159 = `
		INSERT INTO purchase_suppressions (execution_id, account_id, provider, service, region,
		                                   resource_type, engine, suppressed_count, expires_at,
		                                   availability_zone, platform, tenancy, az_config)
		VALUES ($1, 'acct', 'aws', 'relational-db', 'us-east-1', 'db.r5.large', 'mysql', $2,
		        NOW() + INTERVAL '7 days', '', '', '', $3)`
)

// TestMigration_PurchaseSuppressionsAZConfig locks down migration 000159
// (issue #408): rows seeded before the migration, with and without a 000134
// variant, survive it with a NULL az_config (the wildcard backfill rule);
// afterwards Single-AZ and Multi-AZ rows of one execution coexist while a
// duplicate and a variant-less az_config are rejected; down merges the
// az_config rows into one row with the summed count, restores the 000134 key
// and keeps the pre-existing rows; up re-applies cleanly.
func TestMigration_PurchaseSuppressionsAZConfig(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool, path := container.DB.Pool(), getMigrationsPath()
	prev := previousMigrationVersion(t, 159)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))
	// Pre-migration rows: one legacy 6-tuple row (NULL variant) and one RDS
	// row with a 000134 variant, as pre-#408 code writes it.
	_, err := pool.Exec(ctx, `
		INSERT INTO purchase_suppressions (execution_id, account_id, provider, service, region,
		                                   resource_type, engine, suppressed_count, expires_at)
		VALUES ($1, 'acct', 'aws', 'relational-db', 'us-east-1', 'db.t3.micro', 'mysql', 1, NOW() + INTERVAL '7 days')`, exec159)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO purchase_suppressions (execution_id, account_id, provider, service, region,
		                                   resource_type, engine, suppressed_count, expires_at,
		                                   availability_zone, platform, tenancy)
		VALUES ($1, 'acct', 'aws', 'relational-db', 'us-east-1', 'db.r5.large', 'mysql', 2, NOW() + INTERVAL '7 days', '', '', '')`, exec159)
	require.NoError(t, err)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 159))

	var rows, nulls int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE az_config IS NULL)
		FROM purchase_suppressions WHERE execution_id = $1`, exec159).Scan(&rows, &nulls))
	assert.Equal(t, 2, rows, "existing rows survive")
	assert.Equal(t, 2, nulls, "existing rows get a NULL az_config: a wildcard")

	// Single-AZ and Multi-AZ rows coexist next to the NULL row of the same variant.
	for _, v := range []string{"single-az", "multi-az"} {
		_, err = pool.Exec(ctx, insertRDSSuppression159, exec159, 3, v)
		require.NoError(t, err, v)
	}
	_, err = pool.Exec(ctx, insertRDSSuppression159, exec159, 3, "multi-az")
	requireUniqueViolation134(t, err, "purchase_suppressions_execution_variant_azc_key")
	// An az_config without the rest of the variant is rejected.
	_, err = pool.Exec(ctx, `
		INSERT INTO purchase_suppressions (execution_id, account_id, provider, service, region,
		                                   resource_type, engine, suppressed_count, expires_at, az_config)
		VALUES ($1, 'acct', 'aws', 'relational-db', 'us-east-1', 'db.m5.large', 'mysql', 1, NOW() + INTERVAL '7 days', 'multi-az')`, exec159)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "purchase_suppressions_az_config_needs_variant")

	// Down: the NULL, single-az and multi-az rows of db.r5.large merge into one
	// (2 + 3 + 3); the legacy db.t3.micro row is untouched; the column is gone.
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))
	var total int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*), COALESCE(SUM(suppressed_count), 0)
		FROM purchase_suppressions WHERE execution_id = $1 AND resource_type = 'db.r5.large'`, exec159).Scan(&rows, &total))
	assert.Equal(t, 1, rows)
	assert.Equal(t, 8, total)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM purchase_suppressions
		WHERE execution_id = $1 AND resource_type = 'db.t3.micro'`, exec159).Scan(&rows))
	assert.Equal(t, 1, rows)
	var cols int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'purchase_suppressions' AND column_name = 'az_config'`).Scan(&cols))
	assert.Zero(t, cols)
	_, err = pool.Exec(ctx, insertVariantSuppression134, exec159, 1, "us-east-1a", "Linux/UNIX", "default")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, insertVariantSuppression134, exec159, 1, "us-east-1a", "Linux/UNIX", "default")
	requireUniqueViolation134(t, err, "purchase_suppressions_execution_variant_key")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 159), "up must re-apply cleanly after down")
}
