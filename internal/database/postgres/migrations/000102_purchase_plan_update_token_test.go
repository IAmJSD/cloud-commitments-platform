//go:build integration

package migrations_test

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/require"
)

func TestMigration_PurchasePlanUpdateToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := getMigrationsPath()
	container := testhelpers.RequireMigratedDatabase(ctx, t, path)
	pool := container.DB.Pool()

	for _, future := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "future"}[future], func(t *testing.T) {
			tx, beginErr := pool.Begin(ctx)
			require.NoError(t, beginErr)
			defer func() { _ = tx.Rollback(ctx) }()
			initial := time.Now().Truncate(time.Microsecond)
			if future {
				initial = initial.Add(time.Hour)
			}
			var id string
			require.NoError(t, tx.QueryRow(ctx, `INSERT INTO purchase_plans (name, updated_at) VALUES ('token fixture', $1) RETURNING id`, initial).Scan(&id))
			var first, second time.Time
			require.NoError(t, tx.QueryRow(ctx, `UPDATE purchase_plans SET name = 'first' WHERE id = $1 RETURNING updated_at`, id).Scan(&first))
			require.NoError(t, tx.QueryRow(ctx, `UPDATE purchase_plans SET name = 'second' WHERE id = $1 RETURNING updated_at`, id).Scan(&second))
			require.True(t, first.After(initial), "first token must advance, including beyond a future old token")
			require.True(t, second.After(first), "two writes in the same transaction must have distinct tokens")
		})
	}

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, previousMigrationVersion(t, 102)))
	var function string
	require.NoError(t, pool.QueryRow(ctx, `SELECT p.proname FROM pg_trigger t JOIN pg_proc p ON p.oid = t.tgfoid WHERE t.tgname = 'update_purchase_plans_updated_at'`).Scan(&function))
	require.Equal(t, "update_updated_at_column", function)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 102))
	require.NoError(t, pool.QueryRow(ctx, `SELECT p.proname FROM pg_trigger t JOIN pg_proc p ON p.oid = t.tgfoid WHERE t.tgname = 'update_purchase_plans_updated_at'`).Scan(&function))
	require.Equal(t, "update_purchase_plan_timestamp", function)
	require.NoError(t, pool.QueryRow(ctx, `SELECT p.proname FROM pg_trigger t JOIN pg_proc p ON p.oid = t.tgfoid WHERE t.tgname = 'update_purchase_executions_updated_at'`).Scan(&function))
	require.Equal(t, "update_updated_at_column", function, "other tables retain their existing trigger")
}
