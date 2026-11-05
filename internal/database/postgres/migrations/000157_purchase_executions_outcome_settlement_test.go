//go:build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/require"
)

// TestMigration_PurchaseExecutionsOutcomeSettlement covers 000157 and its
// VALIDATE follow-up 000158: up, down and the all-or-nothing CHECK (#999).
func TestMigration_PurchaseExecutionsOutcomeSettlement(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { require.NoError(t, container.Cleanup(context.Background())) })
	pool, path := container.DB.Pool(), getMigrationsPath()
	const version, validateVersion = 157, 158
	prev := previousMigrationVersion(t, version)
	validated := func() bool {
		var ok bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT convalidated FROM pg_constraint
			WHERE conname = 'purchase_executions_outcome_settlement_complete'`).Scan(&ok))
		return ok
	}

	columns := func() int {
		var n int
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.columns
			WHERE table_name = 'purchase_executions'
			  AND column_name IN ('outcome_settled_at', 'outcome_settled_by', 'outcome_settlement')`).Scan(&n))
		return n
	}

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))
	require.Zero(t, columns(), "before 000157 the columns must not exist")
	_, err := pool.Exec(ctx, `
		INSERT INTO purchase_executions (execution_id, status, step_number, scheduled_date, recommendations)
		VALUES ('00000000-0000-0000-0000-0000000009a1', 'failed', 1, now(), '[]')`)
	require.NoError(t, err)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, version))
	require.Equal(t, 3, columns())
	require.False(t, validated(), "000157 adds the CHECK NOT VALID to avoid a table scan under lock")
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

	_, err = pool.Exec(ctx, `UPDATE purchase_executions SET outcome_settled_at = NULL, outcome_settled_by = NULL,
		outcome_settlement = NULL`)
	require.NoError(t, err)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, validateVersion))
	require.True(t, validated(), "000158 validates the CHECK")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))
	require.Zero(t, columns(), "down must drop the columns")
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, validateVersion))
	require.Equal(t, 3, columns(), "up must re-apply cleanly after down")
}

// The database itself refuses to link a retry to a settled row, so a binary
// from before the settlement code cannot undo "a settled row is never retried".
func TestMigration_OutcomeSettlementRefusesRetryLink(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { require.NoError(t, container.Cleanup(context.Background())) })
	pool := container.DB.Pool()
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, getMigrationsPath(), 157))

	insert := func(id, settled string) {
		_, err := pool.Exec(ctx, `INSERT INTO purchase_executions (execution_id, status, step_number, scheduled_date, recommendations,
			outcome_settled_at, outcome_settled_by, outcome_settlement)
			VALUES ($1, 'failed', 1, now(), '[]', `+settled+`)`, id)
		require.NoError(t, err)
	}
	const settledID, openID, succ1, succ2 = "00000000-0000-0000-0000-0000000009b1", "00000000-0000-0000-0000-0000000009b2",
		"00000000-0000-0000-0000-0000000009b3", "00000000-0000-0000-0000-0000000009b4"
	insert(settledID, `now(), 'u', 's'`)
	insert(openID, `NULL, NULL, NULL`)
	insert(succ1, `NULL, NULL, NULL`)
	insert(succ2, `NULL, NULL, NULL`)

	link := `UPDATE purchase_executions SET retry_execution_id = $2 WHERE execution_id = $1`
	_, err := pool.Exec(ctx, link, settledID, succ1)
	require.Error(t, err, "linking a retry to a settled row must fail in the database")
	_, err = pool.Exec(ctx, link, openID, succ2)
	require.NoError(t, err, "an unsettled failed row can still be retried")
}

// Down refuses to drop settlements, which would let a settled row be retried
// and its plan's delete refused again; the refusal leaves the schema intact.
func TestMigration_OutcomeSettlementDownRefusesWithSettlements(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { require.NoError(t, container.Cleanup(context.Background())) })
	pool, path := container.DB.Pool(), getMigrationsPath()
	prev := previousMigrationVersion(t, 157)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 157))
	_, err := pool.Exec(ctx, `INSERT INTO purchase_executions (execution_id, status, step_number, scheduled_date, recommendations,
		outcome_settled_at, outcome_settled_by, outcome_settlement)
		VALUES (gen_random_uuid(), 'failed', 1, now(), '[]', now(), 'u', 's')`)
	require.NoError(t, err)

	require.Error(t, migrations.MigrateToVersion(ctx, pool, path, prev), "down must refuse while a settlement exists")
	var cols, triggers int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'purchase_executions' AND column_name LIKE 'outcome_settle%'`).Scan(&cols))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM pg_trigger
		WHERE tgname = 'purchase_executions_refuse_retry_of_settled'`).Scan(&triggers))
	require.Equal(t, 3, cols, "a refused down leaves the columns")
	require.Equal(t, 1, triggers, "a refused down leaves the trigger")
}
