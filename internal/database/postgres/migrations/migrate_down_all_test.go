//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/require"
)

// TestMigrations_DownAllRoundTrip migrates to the latest version, rolls every
// migration back to version 0 one step at a time (the equivalent of
// `migrate down -all`), then re-applies them all. Each down file must invert
// its own up against the schema the later migrations left behind; a down that
// assumes an object a later migration removed leaves the DB dirty (issue #205).
func TestMigrations_DownAllRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	migrationsPath := getMigrationsPath()

	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, migrationVersionsDesc(t)[0]),
		"migrate up to latest")
	latest, dirty, err := migrations.GetMigrationVersion(ctx, pool, migrationsPath)
	require.NoError(t, err)
	require.False(t, dirty)
	require.Equal(t, migrationVersionsDesc(t)[0], latest)

	for range migrationVersionsDesc(t) {
		before, _, verr := migrations.GetMigrationVersion(ctx, pool, migrationsPath)
		require.NoError(t, verr)
		require.NoError(t, migrations.RollbackMigrations(ctx, pool, migrationsPath, 1),
			"rolling back from version %d must succeed", before)
	}

	version, dirty, err := migrations.GetMigrationVersion(ctx, pool, migrationsPath)
	require.NoError(t, err)
	require.False(t, dirty, "DB must not be dirty after migrating down to 0")
	require.Equal(t, uint(0), version, "all migrations must be rolled back")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, latest),
		"re-applying every migration after a full down must succeed")
}

// TestMigrations_DownToOneRoundTrip rolls back to version 1 rather than 0, so
// the tables 000001 created survive with the rows later migrations seeded,
// then re-applies everything. 000064's down is a no-op by design, so its
// Purchaser at ...0007 is still there when 000059 re-runs, and 000059 refused
// it as a foreign "Purchaser" (issue #316).
func TestMigrations_DownToOneRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	migrationsPath := getMigrationsPath()
	pool := testhelpers.RequireMigratedDatabase(ctx, t, migrationsPath).DB.Pool()
	latest := migrationVersionsDesc(t)[0]

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 1), "roll back to version 1")
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, latest),
		"re-applying every migration after a rollback to version 1 must succeed")
	version, dirty, err := migrations.GetMigrationVersion(ctx, pool, migrationsPath)
	require.NoError(t, err)
	require.False(t, dirty)
	require.Equal(t, latest, version)
}
