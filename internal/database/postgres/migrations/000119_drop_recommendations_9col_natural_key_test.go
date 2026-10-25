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

const insertRecForMigration119 = `
	INSERT INTO recommendations
	    (collected_at, provider, service, region, resource_type, engine, payload,
	     upfront_cost, monthly_savings, term, payment_option)
	VALUES ($1::timestamptz, 'aws', 'ec2', 'us-east-1', $2, '', $3::jsonb, 0, 0, 1, 'all-upfront')
`

// TestMigration_DropRecommendations9ColNaturalKey locks down migration 000119
// (issue #806, PR-E): rows that differ only by platform or tenancy coexist once
// the 9-column index is gone, the 11-column index still dedupes, and the
// rollback recreates the 9-column index after collapsing those duplicates.
func TestMigration_DropRecommendations9ColNaturalKey(t *testing.T) {
	ctx := context.Background()
	migrationsPath := getMigrationsPath()

	container := testhelpers.RequireEmptyDatabase(ctx, t)
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
	const (
		linux   = `{"availability_zone":"us-east-1a","details":{"platform":"Linux/UNIX","tenancy":"default"}}`
		windows = `{"availability_zone":"us-east-1a","details":{"platform":"Windows","tenancy":"default"}}`
		rhel    = `{"availability_zone":"us-east-1a","details":{"platform":"Linux/UNIX","tenancy":"dedicated"}}`
		other   = `{"availability_zone":"us-east-1b","details":{"platform":"Linux/UNIX","tenancy":"default"}}`
	)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, previousMigrationVersion(t, 119)))
	require.True(t, indexExists("recommendations_natural_key_az_idx"))
	require.True(t, indexExists("recommendations_natural_key_az_tp_idx"))

	// A regional row and one EC2 row exist before the migration.
	_, err := pool.Exec(ctx, insertRecForMigration119, "2026-01-01T00:00:00Z", "regional", `{"details":{"scope":"Region"}}`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, insertRecForMigration119, "2026-01-01T00:00:00Z", "ec2", linux)
	require.NoError(t, err)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 119))
	assert.False(t, indexExists("recommendations_natural_key_az_idx"), "9-column index must be dropped")
	assert.True(t, indexExists("recommendations_natural_key_az_tp_idx"), "11-column index must remain")

	// Rows differing only by platform or tenancy now coexist; an exact
	// duplicate and a duplicate regional row are still rejected by the
	// 11-column index.
	_, err = pool.Exec(ctx, insertRecForMigration119, "2026-01-02T00:00:00Z", "ec2", windows)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, insertRecForMigration119, "2026-01-03T00:00:00Z", "ec2", rhel)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, insertRecForMigration119, "2026-01-03T00:00:00Z", "ec2", other)
	require.NoError(t, err, "a different AZ is a different row under both indexes")
	assert.Equal(t, 4, count("ec2"))
	_, err = pool.Exec(ctx, insertRecForMigration119, "2026-01-03T00:00:00Z", "ec2", linux)
	require.Error(t, err, "same platform and tenancy must still violate the 11-column index")
	_, err = pool.Exec(ctx, insertRecForMigration119, "2026-01-03T00:00:00Z", "regional", `{"details":{"scope":"Region"}}`)
	require.Error(t, err, "regional duplicate must still violate the 11-column index")

	// Rollback: platform/tenancy-only duplicates collapse to the newest, the
	// other-AZ row and the regional row are untouched, and the 9-column index
	// is recreated with its original shape.
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, previousMigrationVersion(t, 119)))
	assert.Equal(t, 2, count("ec2"), "only duplicates under the 9-column key may be deleted")
	assert.Equal(t, 1, count("regional"))
	rows, err := pool.Query(ctx, `SELECT availability_zone, platform, tenancy FROM recommendations WHERE resource_type = 'ec2' ORDER BY availability_zone`)
	require.NoError(t, err)
	type kept struct{ az, platform, tenancy string }
	var got []kept
	for rows.Next() {
		var k kept
		require.NoError(t, rows.Scan(&k.az, &k.platform, &k.tenancy))
		got = append(got, k)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []kept{
		{"us-east-1a", "Linux/UNIX", "dedicated"}, // newest of the us-east-1a group
		{"us-east-1b", "Linux/UNIX", "default"},
	}, got)

	var indexDef string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname = 'recommendations_natural_key_az_idx'`).Scan(&indexDef))
	assert.Contains(t, indexDef, "UNIQUE INDEX")
	assert.Contains(t, indexDef, "(account_key, provider, service, region, resource_type, engine, term, payment_option, availability_zone)")
	assert.True(t, indexExists("recommendations_natural_key_az_tp_idx"))

	// Re-applying the up migration after a rollback works.
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 119))
	assert.False(t, indexExists("recommendations_natural_key_az_idx"))
}
