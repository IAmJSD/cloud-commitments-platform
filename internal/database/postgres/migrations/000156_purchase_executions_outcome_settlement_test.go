//go:build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/require"
)

// TestMigration_PurchaseExecutionsOutcomeSettlement covers 000156 up, down and
// the all-or-nothing CHECK (#999).
func TestMigration_PurchaseExecutionsOutcomeSettlement(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { require.NoError(t, container.Cleanup(context.Background())) })
	pool, path := container.DB.Pool(), getMigrationsPath()
	const version = 156
	prev := previousMigrationVersion(t, version)

	columns := func() int {
		var n int
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.columns
			WHERE table_name = 'purchase_executions'
			  AND column_name IN ('outcome_settled_at', 'outcome_settled_by', 'outcome_settlement')`).Scan(&n))
		return n
	}

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))
	require.Zero(t, columns(), "before 000156 the columns must not exist")
	_, err := pool.Exec(ctx, `
		INSERT INTO purchase_executions (execution_id, status, step_number, scheduled_date, recommendations)
		VALUES ('00000000-0000-0000-0000-0000000009a1', 'failed', 1, now(), '[]')`)
	require.NoError(t, err)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, version))
	require.Equal(t, 3, columns())
	var settled bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT outcome_settled_at IS NOT NULL FROM purchase_executions
		WHERE execution_id = '00000000-0000-0000-0000-0000000009a1'`).Scan(&settled))
	require.False(t, settled, "existing rows start unsettled")

	_, err = pool.Exec(ctx, `UPDATE purchase_executions
		SET outcome_settled_at = now(), outcome_settled_by = 'admin-api-key', outcome_settlement = 'operator:settled'
		WHERE execution_id = '00000000-0000-0000-0000-0000000009a1'`)
	require.NoError(t, err, "a complete settlement is valid")
	for name, set := range map[string]string{
		"time only":   `outcome_settled_at = now()`,
		"no record":   `outcome_settled_at = now(), outcome_settled_by = 'u'`,
		"no actor":    `outcome_settled_at = now(), outcome_settlement = 's'`,
		"record only": `outcome_settlement = 's'`,
	} {
		_, err = pool.Exec(ctx, `INSERT INTO purchase_executions (execution_id, status, step_number, scheduled_date, recommendations)
			VALUES (gen_random_uuid(), 'failed', 1, now(), '[]')`)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `UPDATE purchase_executions SET `+set+` WHERE outcome_settled_at IS NULL AND outcome_settlement IS NULL`)
		require.Error(t, err, "CHECK must reject a partial settlement (%s)", name)
	}

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))
	require.Zero(t, columns(), "down must drop the columns")
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, version))
	require.Equal(t, 3, columns(), "up must re-apply cleanly after down")
}
