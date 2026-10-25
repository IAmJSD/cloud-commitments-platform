//go:build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/require"
)

// TestMigration_SessionPasswordVersion covers 000138 up, the backfill and down (#398).
func TestMigration_SessionPasswordVersion(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { require.NoError(t, container.Cleanup(context.Background())) })
	pool, path := container.DB.Pool(), getMigrationsPath()

	hasColumn := func() bool {
		var n int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
			WHERE table_name = 'sessions' AND column_name = 'password_version'`).Scan(&n))
		return n == 1
	}

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, previousMigrationVersion(t, 138)))
	require.False(t, hasColumn())

	var userID string
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO users (email, password_hash, salt, group_ids)
		VALUES ('session-version@example.com', 'hash-1', '', ARRAY[$1::uuid]) RETURNING id`,
		adminGroupIDForMinAdminTest).Scan(&userID))
	_, err := pool.Exec(ctx, `UPDATE users SET password_hash = 'hash-2' WHERE id = $1`, userID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE users SET password_hash = 'hash-3' WHERE id = $1`, userID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO sessions (token, user_id, email, expires_at)
		VALUES ('existing-session', $1, 'session-version@example.com', NOW() + interval '1 hour')`, userID)
	require.NoError(t, err)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 138))
	var version int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT password_version FROM sessions WHERE token = 'existing-session'`).Scan(&version))
	require.Equal(t, int64(2), version, "an existing session takes its owner's current version")

	_, err = pool.Exec(ctx, `INSERT INTO sessions (token, user_id, email, expires_at)
		VALUES ('writer-without-version', $1, 'session-version@example.com', NOW() + interval '1 hour')`, userID)
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx, `SELECT password_version FROM sessions WHERE token = 'writer-without-version'`).Scan(&version))
	require.Zero(t, version, "a writer that omits the version gets 0, which fails closed once the user has rotated")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, previousMigrationVersion(t, 138)))
	require.False(t, hasColumn())
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 138))
	require.True(t, hasColumn())
}
