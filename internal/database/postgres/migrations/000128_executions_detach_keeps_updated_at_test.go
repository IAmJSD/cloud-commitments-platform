//go:build integration

package migrations_test

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigration_ExecutionsDetachKeepsUpdatedAt locks down migration 000128:
// an UPDATE that only detaches a purchase_executions row from its cloud
// account keeps updated_at, every other UPDATE still stamps NOW(), and the
// down migration restores the stamp-everything trigger (issue #265).
func TestMigration_ExecutionsDetachKeepsUpdatedAt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool, path := container.DB.Pool(), getMigrationsPath()

	const acct = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)

	// seed inserts a failed execution on acct with an old updated_at (an
	// INSERT does not fire the BEFORE UPDATE trigger) and returns its id.
	seed := func(t *testing.T) string {
		t.Helper()
		var id string
		require.NoError(t, pool.QueryRow(ctx, `
			INSERT INTO purchase_executions (status, step_number, scheduled_date, cloud_account_id, updated_at)
			VALUES ('failed', 1, NOW(), $1, $2)
			RETURNING execution_id::text`, acct, old).Scan(&id))
		return id
	}
	updatedAt := func(t *testing.T, id string) time.Time {
		t.Helper()
		var ts time.Time
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT updated_at FROM purchase_executions WHERE execution_id = $1`, id).Scan(&ts))
		return ts.UTC()
	}
	exec := func(t *testing.T, sql, id string) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, id)
		require.NoError(t, err)
	}

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 128))
	_, err := pool.Exec(ctx, `
		INSERT INTO cloud_accounts (id, name, provider, external_id)
		VALUES ($1, 'detach-test', 'azure', $2)`, acct, acct)
	require.NoError(t, err)

	t.Run("detach only keeps updated_at", func(t *testing.T) {
		id := seed(t)
		exec(t, `UPDATE purchase_executions SET cloud_account_id = NULL WHERE execution_id = $1`, id)
		assert.True(t, updatedAt(t, id).Equal(old), "a detach-only UPDATE must keep updated_at")
	})

	t.Run("detach plus another change stamps NOW", func(t *testing.T) {
		id := seed(t)
		exec(t, `UPDATE purchase_executions SET cloud_account_id = NULL, error = 'x' WHERE execution_id = $1`, id)
		assert.True(t, updatedAt(t, id).After(old), "a detach that also changes another column must stamp NOW()")
	})

	t.Run("ordinary update stamps NOW", func(t *testing.T) {
		id := seed(t)
		exec(t, `UPDATE purchase_executions SET status = 'canceled' WHERE execution_id = $1`, id)
		assert.True(t, updatedAt(t, id).After(old), "a status change must stamp NOW()")
	})

	t.Run("attach stamps NOW", func(t *testing.T) {
		id := seed(t)
		exec(t, `UPDATE purchase_executions SET cloud_account_id = NULL WHERE execution_id = $1`, id)
		_, err := pool.Exec(ctx, `UPDATE purchase_executions SET cloud_account_id = $2 WHERE execution_id = $1`, id, acct)
		require.NoError(t, err)
		assert.True(t, updatedAt(t, id).After(old), "setting an account must stamp NOW()")
	})

	t.Run("down restores the shared trigger", func(t *testing.T) {
		id := seed(t)
		require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, previousMigrationVersion(t, 128)))
		exec(t, `UPDATE purchase_executions SET cloud_account_id = NULL WHERE execution_id = $1`, id)
		assert.True(t, updatedAt(t, id).After(old), "after the down migration a detach stamps NOW() again")

		var n int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM pg_proc WHERE proname = 'purchase_executions_set_updated_at'`).Scan(&n))
		assert.Zero(t, n, "down must drop the 000128 trigger function")
	})
}
