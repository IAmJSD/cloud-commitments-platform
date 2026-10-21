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

// TestMigration_RIExchangeDirectExecutions pins migration 000131 (issue #82)
// against a real PostgreSQL: the audit table appears, a minimal row defaults
// to a generated id, 'submitted' and the database clock, the up statement is
// idempotent, the approval flow's ri_exchange_history is untouched, and the
// down migration drops the table.
func TestMigration_RIExchangeDirectExecutions(t *testing.T) {
	ctx := context.Background()
	migrationsPath := getMigrationsPath()

	container, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	defer container.Cleanup(ctx)
	pool := container.DB.Pool()

	tableExists := func() bool {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass('ri_exchange_direct_executions') IS NOT NULL`).Scan(&exists))
		return exists
	}
	historyColumns := func() int {
		var n int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.columns WHERE table_name = 'ri_exchange_history'`).Scan(&n))
		return n
	}

	prev := previousMigrationVersion(t, 131)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, prev))
	require.False(t, tableExists())
	historyBefore := historyColumns()

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 131))
	require.True(t, tableExists())
	assert.Equal(t, historyBefore, historyColumns(), "the approval flow's ledger table must not change")

	var status string
	var completedNull, createdSet bool
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO ri_exchange_direct_executions (provider, source_ids, targets, max_payment_due, max_payment_currency, executed_by)
		VALUES ('azure', ARRAY['res-1'], '[{"sku":"Standard_D4s_v3"}]'::jsonb, 100, 'USD', 'alice@example.com')
		RETURNING status, completed_at IS NULL, created_at IS NOT NULL`).Scan(&status, &completedNull, &createdSet))
	assert.Equal(t, "submitted", status)
	assert.True(t, completedNull)
	assert.True(t, createdSet)

	up, err := os.ReadFile(filepath.Join(migrationsPath, "000131_ri_exchange_direct_executions.up.sql"))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(up))
	require.NoError(t, err, "the up statement must be idempotent")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, prev))
	assert.False(t, tableExists(), "down migration must drop the table")
	assert.Equal(t, historyBefore, historyColumns())
}
