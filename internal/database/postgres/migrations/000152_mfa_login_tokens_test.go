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

// Migration 000152 adds mfa_login_tokens (issue #134): one row per pending
// MFA login, keyed by the token's hash, removed with its user, and dropped by
// the down migration.
func TestMigration_MFALoginTokens(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	migrationsPath := getMigrationsPath()

	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, previousMigrationVersion(t, 152)))
	owner := seedDeactivatedAtUser(ctx, t, pool, "mfa-token-owner@example.com", true, nil, nil)
	seedDeactivatedAtUser(ctx, t, pool, "mfa-token-keeper@example.com", true, nil, nil) // keeps an admin after the delete below

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 152))
	insert := `INSERT INTO mfa_login_tokens (token_hash, user_id, password_version, attempts, expires_at)
		VALUES ($1, $2, 0, $3, NOW() + interval '5 minutes')`
	_, err := pool.Exec(ctx, insert, "hash-1", owner, 0)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, insert, "hash-1", owner, 0)
	require.Error(t, err, "token_hash is unique")
	_, err = pool.Exec(ctx, insert, "", owner, 0)
	require.Error(t, err, "an empty hash is refused")
	_, err = pool.Exec(ctx, insert, "hash-2", owner, -1)
	require.Error(t, err, "attempts cannot go negative")

	_, err = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, owner)
	require.NoError(t, err)
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM mfa_login_tokens`).Scan(&n))
	assert.Zero(t, n, "a user's tokens go with the user")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, previousMigrationVersion(t, 152)))
	var exists bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass('mfa_login_tokens') IS NOT NULL`).Scan(&exists))
	assert.False(t, exists, "the down migration drops the table")
}
