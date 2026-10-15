//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigration_PurchaseExecutionFanOutShapes pins migration 000123 (issue #97)
// against a real PostgreSQL: a lineage with a per-account row is backfilled as
// "multi", a lineage without one is left unpinned, the up statement is
// idempotent, and the down migration drops the table.
func TestMigration_PurchaseExecutionFanOutShapes(t *testing.T) {
	ctx := context.Background()
	migrationsPath := getMigrationsPath()

	container, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	defer container.Cleanup(ctx)
	pool := container.DB.Pool()

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, previousMigrationVersion(t, 123)))

	fannedOut, singleOnly := uuid.NewString(), uuid.NewString()
	for _, key := range []string{fannedOut, fannedOut + ":" + uuid.NewString(), singleOnly} {
		_, err = pool.Exec(ctx, `
			INSERT INTO purchase_executions (status, scheduled_date, idempotency_key)
			VALUES ('failed', NOW(), $1)`, key)
		require.NoError(t, err)
	}

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 123))

	var shape string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT shape FROM purchase_execution_fan_out_shapes WHERE idempotency_key = $1`, fannedOut).Scan(&shape))
	assert.Equal(t, "multi", shape, "a lineage with a per-account row fanned out")

	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM purchase_execution_fan_out_shapes WHERE idempotency_key = $1`, singleOnly).Scan(&n))
	assert.Zero(t, n, "a lineage without a per-account row must not be guessed")
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM purchase_execution_fan_out_shapes`).Scan(&n))
	assert.Equal(t, 1, n, "only the root lineage is pinned, never a per-account key")

	up, err := os.ReadFile(filepath.Join(migrationsPath, "000123_purchase_execution_fan_out_shapes.up.sql"))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(up))
	require.NoError(t, err, "the up statement must be idempotent")

	_, err = pool.Exec(ctx, `INSERT INTO purchase_execution_fan_out_shapes (idempotency_key, shape) VALUES ($1, 'both')`, uuid.NewString())
	assert.Error(t, err, "only single and multi are valid shapes")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, previousMigrationVersion(t, 123)))
	var exists bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass('purchase_execution_fan_out_shapes') IS NOT NULL`).Scan(&exists))
	assert.False(t, exists, "down migration must drop the table")
}
