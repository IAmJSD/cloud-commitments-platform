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

// Migration 000154 adds the nullable purchase_executions.first_dispatch_at:
// existing rows stay NULL and the down migration drops it.
func TestMigration_0154_FirstDispatchAt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	migrationsPath := getMigrationsPath()

	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()

	columnExists := func() bool {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'purchase_executions' AND column_name = 'first_dispatch_at')`).Scan(&exists))
		return exists
	}

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, previousMigrationVersion(t, 154)))
	assert.False(t, columnExists(), "column absent before 000154")
	_, err := pool.Exec(ctx, `INSERT INTO purchase_executions (status, scheduled_date) VALUES ('pending', NOW())`)
	require.NoError(t, err)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 154))
	assert.True(t, columnExists(), "column present after 000154")
	var nullRows, total int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE first_dispatch_at IS NULL), count(*) FROM purchase_executions`).Scan(&nullRows, &total))
	assert.Equal(t, 1, total)
	assert.Equal(t, total, nullRows, "existing rows stay NULL")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, previousMigrationVersion(t, 154)))
	assert.False(t, columnExists(), "the down migration drops the column")
}
