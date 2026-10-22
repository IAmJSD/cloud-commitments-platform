//go:build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/require"
)

// TestMigration_RecommendationsScanKeyIndex checks that 000142 creates the
// keyset index the paginated recommendations scan relies on (issue #981), in
// byte-wise ("C") order with the primary key as tie-breaker, and that the down
// migration removes it.
func TestMigration_RecommendationsScanKeyIndex(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { require.NoError(t, container.Cleanup(context.Background())) })
	pool, path := container.DB.Pool(), getMigrationsPath()

	const version = 142
	prev := previousMigrationVersion(t, version)

	indexDef := func() string {
		t.Helper()
		var def string
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT COALESCE((SELECT indexdef FROM pg_indexes
			                 WHERE tablename = 'recommendations'
			                   AND indexname = 'recommendations_scan_key_idx'), '')`).Scan(&def))
		return def
	}

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))
	require.Empty(t, indexDef(), "before 000142 the index must not exist")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, version))
	def := indexDef()
	require.Contains(t, def, `COALESCE((payload ->> 'id'::text), ''::text) COLLATE "C", id)`,
		"the index must be on the byte-wise id, then the primary key")
	require.NotContains(t, def, "UNIQUE", "rows may share an application id; the primary key breaks ties")
	require.NotContains(t, def, "WHERE", "a partial index cannot serve an unfiltered scan")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))
	require.Empty(t, indexDef(), "down must drop the index")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, version))
	require.NotEmpty(t, indexDef(), "up must re-apply cleanly after down")
}
