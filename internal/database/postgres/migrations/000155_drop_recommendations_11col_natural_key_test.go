//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const insertRecForMigration153 = `
	INSERT INTO recommendations
	    (collected_at, provider, service, region, resource_type, engine, payload,
	     upfront_cost, monthly_savings, term, payment_option)
	VALUES ($1::timestamptz, 'aws', 'rds', $2, 'db.r5.large', 'mysql', $3::jsonb, 0, 0, 1, 'all-upfront')
`

// TestMigration_DropRecommendations11ColNaturalKey locks down migration 000155
// (issue #408, contract step): Single-AZ and Multi-AZ rows of the same class
// coexist once the 11-column and 000120 source indexes are gone, the 12-column index still dedupes,
// and the rollback recreates both indexes after collapsing those
// duplicates to the newest row.
func TestMigration_DropRecommendations11ColNaturalKey(t *testing.T) {
	ctx := context.Background()
	migrationsPath := getMigrationsPath()

	container, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	defer container.Cleanup(ctx)
	pool := container.DB.Pool()

	indexExists := func(name string) bool {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE tablename = 'recommendations' AND indexname = $1)`, name).Scan(&exists))
		return exists
	}
	count := func(region string) int {
		var n int
		require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM recommendations WHERE region = $1`, region).Scan(&n))
		return n
	}
	const (
		single = `{"details":{"engine":"mysql","az_config":"single-az"}}`
		multi  = `{"details":{"engine":"mysql","az_config":"multi-az"}}`
		none   = `{"details":{"engine":"mysql"}}`
	)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, previousMigrationVersion(t, 155)))
	require.True(t, indexExists("recommendations_natural_key_az_tp_idx"))
	require.True(t, indexExists("recommendations_natural_key_az_tp_azc_idx"))
	require.True(t, indexExists("recommendations_natural_key_source_idx"))
	require.True(t, indexExists("recommendations_natural_key_source_azc_idx"))

	_, err = pool.Exec(ctx, insertRecForMigration153, "2026-01-01T00:00:00Z", "us-east-1", single)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, insertRecForMigration153, "2026-01-01T00:00:00Z", "us-west-2", single)
	require.NoError(t, err)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 155))
	assert.False(t, indexExists("recommendations_natural_key_az_tp_idx"), "11-column index must be dropped")
	assert.True(t, indexExists("recommendations_natural_key_az_tp_azc_idx"), "12-column index must remain")
	assert.False(t, indexExists("recommendations_natural_key_source_idx"), "000120's source index (no az_config) must be dropped")
	assert.True(t, indexExists("recommendations_natural_key_source_azc_idx"), "the source-aware key with az_config must remain")

	// Rows differing only by az_config now coexist; an exact duplicate is
	// still rejected by the 12-column index.
	_, err = pool.Exec(ctx, insertRecForMigration153, "2026-01-02T00:00:00Z", "us-east-1", multi)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, insertRecForMigration153, "2026-01-03T00:00:00Z", "us-east-1", none)
	require.NoError(t, err)
	assert.Equal(t, 3, count("us-east-1"))
	_, err = pool.Exec(ctx, insertRecForMigration153, "2026-01-04T00:00:00Z", "us-east-1", single)
	require.Error(t, err, "the same az_config must still violate the 12-column index")

	// Rollback: az_config-only duplicates collapse to the newest, the other
	// region's row is untouched, and the 11-column index is recreated with
	// its original shape.
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, previousMigrationVersion(t, 155)))
	assert.Equal(t, 1, count("us-east-1"), "only duplicates under the 11-column key may be deleted")
	assert.Equal(t, 1, count("us-west-2"))
	var kept string
	require.NoError(t, pool.QueryRow(ctx, `SELECT az_config FROM recommendations WHERE region = 'us-east-1'`).Scan(&kept))
	assert.Equal(t, "", kept, "the most recently collected row survives")

	var indexDef string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname = 'recommendations_natural_key_az_tp_idx'`).Scan(&indexDef))
	assert.Contains(t, indexDef, "UNIQUE INDEX")
	assert.Contains(t, indexDef, "(account_key, provider, service, region, resource_type, engine, term, payment_option, availability_zone, platform, tenancy)")
	assert.True(t, indexExists("recommendations_natural_key_az_tp_azc_idx"))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname = 'recommendations_natural_key_source_idx'`).Scan(&indexDef))
	assert.Contains(t, indexDef, "UNIQUE INDEX")
	assert.Contains(t, indexDef, "(account_key, provider, service, region, resource_type, engine, term, payment_option, availability_zone, platform, tenancy, source_account_key)")

	// Re-applying the up migration after a rollback works.
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 155))
	assert.False(t, indexExists("recommendations_natural_key_az_tp_idx"))
	assert.False(t, indexExists("recommendations_natural_key_source_idx"))
}
