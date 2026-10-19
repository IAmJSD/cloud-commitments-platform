//go:build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/require"
)

// TestMigration_PurchaseExecutionsCreatedBySystem covers 000130 up, down and the CHECK (#233).
func TestMigration_PurchaseExecutionsCreatedBySystem(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { require.NoError(t, container.Cleanup(context.Background())) })
	pool, path := container.DB.Pool(), getMigrationsPath()

	hasColumn := func() bool {
		var n int
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.columns
			WHERE table_name = 'purchase_executions' AND column_name = 'created_by_system'`).Scan(&n))
		return n == 1
	}

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 127))
	require.False(t, hasColumn(), "before 000130 the column must not exist")

	_, err := pool.Exec(ctx, `
		INSERT INTO purchase_executions (execution_id, status, step_number, scheduled_date, recommendations)
		VALUES ('00000000-0000-0000-0000-0000000000a1', 'pending', 1, now(), '[]')`)
	require.NoError(t, err)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 130))
	require.True(t, hasColumn())

	var flag bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT created_by_system FROM purchase_executions
		WHERE execution_id = '00000000-0000-0000-0000-0000000000a1'`).Scan(&flag))
	require.False(t, flag, "existing rows default to false")

	_, err = pool.Exec(ctx, `
		INSERT INTO purchase_executions (execution_id, status, step_number, scheduled_date, recommendations, created_by_system)
		VALUES ('00000000-0000-0000-0000-0000000000a2', 'pending', 1, now(), '[]', true)`)
	require.NoError(t, err, "system-created row without a creator is valid")

	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, email, password_hash, salt, active, group_ids, created_at, updated_at)
		VALUES ('00000000-0000-0000-0000-0000000000b1', 'u@example.com', '', '', true, ARRAY[gen_random_uuid()], NOW(), NOW())`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO purchase_executions (execution_id, status, step_number, scheduled_date, recommendations,
		                                 created_by_system, created_by_user_id)
		VALUES ('00000000-0000-0000-0000-0000000000a3', 'pending', 1, now(), '[]', true,
		        '00000000-0000-0000-0000-0000000000b1')`)
	require.Error(t, err, "CHECK must reject a flagged row that names a human creator")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 127))
	require.False(t, hasColumn(), "down must drop the column")
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 130))
	require.True(t, hasColumn(), "up must re-apply cleanly after down")
}
