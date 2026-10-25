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
