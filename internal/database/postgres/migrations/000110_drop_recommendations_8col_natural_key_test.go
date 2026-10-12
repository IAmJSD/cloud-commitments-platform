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

const insertRecForMigration110 = `
	INSERT INTO recommendations
	    (collected_at, provider, service, region, resource_type, engine, payload,
	     upfront_cost, monthly_savings, term, payment_option)
	VALUES ($1::timestamptz, 'aws', 'ec2', 'us-east-1', $2, '', $3::jsonb, 0, 0, 1, 'all-upfront')
`

// TestMigration_DropRecommendations8ColNaturalKey locks down migration 000110
// (issue #792, PR2c): rows that differ only by availability zone coexist once
// the 8-column index is gone, the 9-column index still dedupes per AZ, and the
// rollback recreates the 8-column index after collapsing AZ-only duplicates.
func TestMigration_DropRecommendations8ColNaturalKey(t *testing.T) {
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
	count := func(resourceType string) int {
		var n int
		require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM recommendations WHERE resource_type = $1`, resourceType).Scan(&n))
		return n
	}

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 109))
	require.True(t, indexExists("recommendations_natural_key_idx"))

	// A regional row and one zonal row exist before the migration.
	_, err = pool.Exec(ctx, insertRecForMigration110, "2026-01-01T00:00:00Z", "regional", `{"details":{"scope":"Region"}}`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, insertRecForMigration110, "2026-01-01T00:00:00Z", "zonal", `{"availability_zone":"us-east-1a"}`)
	require.NoError(t, err)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 110))
	assert.False(t, indexExists("recommendations_natural_key_idx"), "8-column index must be dropped")
	assert.True(t, indexExists("recommendations_natural_key_az_idx"), "9-column index must remain")

	// Two zonal rows differing only by AZ now coexist; a same-AZ duplicate and a
	// duplicate regional row are still rejected by the 9-column index.
	_, err = pool.Exec(ctx, insertRecForMigration110, "2026-01-02T00:00:00Z", "zonal", `{"availability_zone":"us-east-1b"}`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, insertRecForMigration110, "2026-01-03T00:00:00Z", "zonal", `{"availability_zone":"us-east-1c"}`)
	require.NoError(t, err)
	assert.Equal(t, 3, count("zonal"))
	_, err = pool.Exec(ctx, insertRecForMigration110, "2026-01-03T00:00:00Z", "zonal", `{"availability_zone":"us-east-1a"}`)
	require.Error(t, err, "same-AZ duplicate must still violate the 9-column index")
	_, err = pool.Exec(ctx, insertRecForMigration110, "2026-01-03T00:00:00Z", "regional", `{"details":{"scope":"Region"}}`)
	require.Error(t, err, "regional duplicate must still violate the 9-column index")

	// Rollback: AZ-only duplicates collapse to the newest, the regional row is
	// untouched, and the 8-column index is recreated with its original shape.
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 109))
	assert.Equal(t, 1, count("zonal"), "AZ-only duplicates must be collapsed so the 8-column index can be rebuilt")
	assert.Equal(t, 1, count("regional"))
	var az string
	require.NoError(t, pool.QueryRow(ctx, `SELECT availability_zone FROM recommendations WHERE resource_type = 'zonal'`).Scan(&az))
	assert.Equal(t, "us-east-1c", az, "the most recently collected row must survive")

	var indexDef string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname = 'recommendations_natural_key_idx'`).Scan(&indexDef))
	assert.Contains(t, indexDef, "UNIQUE INDEX")
	assert.Contains(t, indexDef, "(account_key, provider, service, region, resource_type, engine, term, payment_option)")
	assert.True(t, indexExists("recommendations_natural_key_az_idx"))

	// Re-applying the up migration after a rollback works.
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 110))
	assert.False(t, indexExists("recommendations_natural_key_idx"))
}
