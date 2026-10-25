//go:build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/require"
)

func TestMigration_APIKeyPasswordVersion(t *testing.T) {
	ctx := context.Background()
	path := getMigrationsPath()
	container := testhelpers.RequireMigratedDatabase(ctx, t, path)
	pool := container.DB.Pool()

	var id string
	var version int64
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO users (email, password_hash, salt, group_ids)
		VALUES ('password-version@example.com', 'hash-1', '', ARRAY[$1::uuid]) RETURNING id, password_version`,
		adminGroupIDForMinAdminTest).Scan(&id, &version))
	require.Zero(t, version)
	require.NoError(t, pool.QueryRow(ctx, `UPDATE users SET email = 'renamed@example.com', password_hash = 'hash-1'
		WHERE id = $1 RETURNING password_version`, id).Scan(&version))
	require.Zero(t, version, "rewriting the same hash must not bump the version")
	require.NoError(t, pool.QueryRow(ctx, `UPDATE users SET password_hash = 'hash-2', password_version = 0
		WHERE id = $1 RETURNING password_version`, id).Scan(&version))
	require.Equal(t, int64(1), version, "the trigger owns the version, whatever the writer sets")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, previousMigrationVersion(t, 103)))
	var columns int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE column_name = 'password_version' AND table_name IN ('users', 'api_keys')`).Scan(&columns))
	require.Zero(t, columns)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 103))
}
