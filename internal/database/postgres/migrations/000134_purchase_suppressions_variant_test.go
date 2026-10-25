//go:build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	exec134A = "00000000-0000-0000-0000-000000134a01"
	exec134B = "00000000-0000-0000-0000-000000134b01"

	// insertLegacySuppression134 is the insert pre-#816 code issues: the
	// 6-tuple only, never the variant columns.
	insertLegacySuppression134 = `
		INSERT INTO purchase_suppressions (execution_id, account_id, provider, service, region,
		                                   resource_type, engine, suppressed_count, expires_at)
		VALUES ($1, 'acct', 'aws', 'ec2', 'us-east-1', $2, '', $3, NOW() + INTERVAL '7 days')`

	insertVariantSuppression134 = `
		INSERT INTO purchase_suppressions (execution_id, account_id, provider, service, region,
		                                   resource_type, engine, suppressed_count, expires_at,
		                                   availability_zone, platform, tenancy)
		VALUES ($1, 'acct', 'aws', 'ec2', 'us-east-1', 'm5.large', '', $2, NOW() + INTERVAL '7 days', $3, $4, $5)`
)

func requireUniqueViolation134(t *testing.T, err error, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "23505", pgErr.Code)
	assert.Equal(t, constraint, pgErr.ConstraintName)
}

// TestMigration_PurchaseSuppressionsVariant locks down migration 000134
// (issue #816): nullable availability_zone / platform / tenancy columns, the
// all-or-none CHECK, the widened UNIQUE key (legacy NULL rows stay unique per
// execution and 6-tuple), old-code inserts still working, and a down that
// merges variant rows back into one row per execution and 6-tuple.
func TestMigration_PurchaseSuppressionsVariant(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool, path := container.DB.Pool(), getMigrationsPath()
	prev := previousMigrationVersion(t, 134)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))
	_, err := pool.Exec(ctx, insertLegacySuppression134, exec134A, "m5.large", 2)
	require.NoError(t, err)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 134))

	// A row from before the migration has no variant (NULL in all three).
	var nulls int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM purchase_suppressions
		WHERE availability_zone IS NULL AND platform IS NULL AND tenancy IS NULL`).Scan(&nulls))
	assert.Equal(t, 1, nulls)

	// Old code keeps working after the migration: its insert leaves the
	// variant NULL, and a second legacy row on the same execution and 6-tuple
	// is still rejected (NULLS NOT DISTINCT).
	_, err = pool.Exec(ctx, insertLegacySuppression134, exec134A, "c5.large", 1)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, insertLegacySuppression134, exec134A, "c5.large", 1)
	requireUniqueViolation134(t, err, "purchase_suppressions_execution_variant_key")

	// One execution can now hold rows that differ only by AZ, platform or
	// tenancy, including the known-empty regional AZ.
	for _, v := range [][3]string{
		{"us-east-1a", "Linux/UNIX", "default"},
		{"us-east-1b", "Linux/UNIX", "default"},
		{"", "Linux/UNIX", "default"},
		{"us-east-1a", "Windows", "default"},
		{"us-east-1a", "Linux/UNIX", "dedicated"},
	} {
		_, err = pool.Exec(ctx, insertVariantSuppression134, exec134B, 1, v[0], v[1], v[2])
		require.NoError(t, err, "variant %v", v)
	}
	_, err = pool.Exec(ctx, insertVariantSuppression134, exec134B, 1, "us-east-1a", "Linux/UNIX", "default")
	requireUniqueViolation134(t, err, "purchase_suppressions_execution_variant_key")

	// The CHECK keeps the three columns all-NULL or all-set.
	_, err = pool.Exec(ctx, insertVariantSuppression134, exec134B, 1, "us-east-1c", nil, "default")
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "23514", pgErr.Code)
	assert.Equal(t, "purchase_suppressions_variant_all_or_none", pgErr.ConstraintName)

	// Down merges exec134B's five m5.large variant rows into one 6-tuple row
	// with the summed count, and restores the 6-tuple UNIQUE constraint.
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))
	var rows, total int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*), COALESCE(SUM(suppressed_count), 0)
		FROM purchase_suppressions WHERE execution_id = $1`, exec134B).Scan(&rows, &total))
	assert.Equal(t, 1, rows)
	assert.Equal(t, 5, total)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM purchase_suppressions WHERE execution_id = $1`, exec134A).Scan(&rows))
	assert.Equal(t, 2, rows, "legacy rows on distinct 6-tuples are untouched")
	var cols int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'purchase_suppressions' AND column_name IN ('availability_zone', 'platform', 'tenancy')`).Scan(&cols))
	assert.Zero(t, cols, "down drops the variant columns")
	_, err = pool.Exec(ctx, insertLegacySuppression134, exec134B, "m5.large", 1)
	requireUniqueViolation134(t, err, "purchase_suppressions_execution_id_account_id_provider_serv_key")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 134), "up must re-apply cleanly after down")
}
