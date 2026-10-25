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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigration_PurchaseExecutionsNoSubmission pins migration 000113 against a
// real PostgreSQL: a pre-existing execution row gets no_submission = FALSE
// (no proof, fail closed), the up statement is idempotent, and the down
// migration drops the column.
func TestMigration_PurchaseExecutionsNoSubmission(t *testing.T) {
	ctx := context.Background()
	migrationsPath := getMigrationsPath()

	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, previousMigrationVersion(t, 113)))

	var execID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO purchase_executions (status, scheduled_date)
		VALUES ('failed', NOW())
		RETURNING execution_id`).Scan(&execID))

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 113))

	var proof bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT no_submission FROM purchase_executions WHERE execution_id = $1`, execID).Scan(&proof))
	assert.False(t, proof, "an existing row must have no proof")

	up, err := os.ReadFile(filepath.Join(migrationsPath, "000113_purchase_executions_no_submission.up.sql"))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(up))
	require.NoError(t, err, "the up statement must be idempotent")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 112))
	var cols int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'purchase_executions' AND column_name = 'no_submission'`).Scan(&cols))
	assert.Equal(t, 0, cols, "down migration must drop the column")
}
