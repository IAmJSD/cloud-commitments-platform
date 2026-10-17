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

// TestMigration_RecommendationsStatePartial locks down migration 000129:
// recommendations_state gains a nullable last_collection_partial JSONB
// column (issue #87: incomplete multi-subscription sweeps visible outside
// the logs), the seeded singleton row starts out NULL (= complete), and the
// down migration removes the column cleanly.
func TestMigration_RecommendationsStatePartial(t *testing.T) {
	ctx := context.Background()
	migrationsPath := getMigrationsPath()

	container, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	defer container.Cleanup(ctx)
	pool := container.DB.Pool()

	const version = 129
	// Migration numbers are not contiguous, so pin at the real predecessor
	// rather than version-1.
	prev := previousMigrationVersion(t, version)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, prev))

	columnExists := func() bool {
		t.Helper()
		var exists bool
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				 WHERE table_name = 'recommendations_state'
				   AND column_name = 'last_collection_partial'
			)
		`).Scan(&exists))
		return exists
	}
	assert.False(t, columnExists(), "last_collection_partial must not exist before 000129")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, version))

	var dataType, isNullable string
	err = pool.QueryRow(ctx, `
		SELECT data_type, is_nullable
		  FROM information_schema.columns
		 WHERE table_name = 'recommendations_state'
		   AND column_name = 'last_collection_partial'
	`).Scan(&dataType, &isNullable)
	require.NoError(t, err, "last_collection_partial must exist after 000129")
	assert.Equal(t, "jsonb", dataType)
	assert.Equal(t, "YES", isNullable)

	var partialIsNull bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT last_collection_partial IS NULL FROM recommendations_state WHERE id = 1`).Scan(&partialIsNull))
	assert.True(t, partialIsNull, "an existing deployment must read as complete until a partial sweep is recorded")

	// Rollback restores the pre-migration schema, even with a value stored.
	_, err = pool.Exec(ctx,
		`UPDATE recommendations_state SET last_collection_partial = '{"providers":[]}'::jsonb WHERE id = 1`)
	require.NoError(t, err)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, prev))
	assert.False(t, columnExists(), "last_collection_partial must be dropped after rollback")
}
