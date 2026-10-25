//go:build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/require"
)

func TestMigration_LockDefaultUserGroups(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool, path := container.DB.Pool(), getMigrationsPath()
	ids := []string{
		"00000000-0000-5000-8000-000000000005", // Standard Users
		"00000000-0000-5000-8000-000000000006", // Read-Only Users
	}
	const otherID = "00000000-0000-5000-8000-000000000007" // Purchaser, already locked by its own seed

	locked := func(id string) bool {
		var v bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT system_managed FROM groups WHERE id=$1`, id).Scan(&v))
		return v
	}

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 124))
	for _, id := range ids {
		require.False(t, locked(id), "before 000125 %s must be editable", id)
	}
	// An admin-renamed group is still locked by id, with its permissions kept.
	_, err := pool.Exec(ctx, `UPDATE groups SET name='Renamed Standard' WHERE id=$1`, ids[0])
	require.NoError(t, err)
	var permsBefore string
	require.NoError(t, pool.QueryRow(ctx, `SELECT permissions::text FROM groups WHERE id=$1`, ids[0]).Scan(&permsBefore))

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 125))
	for _, id := range ids {
		require.True(t, locked(id), "after 000125 %s must be system-managed", id)
	}
	var name, permsAfter string
	require.NoError(t, pool.QueryRow(ctx, `SELECT name, permissions::text FROM groups WHERE id=$1`, ids[0]).Scan(&name, &permsAfter))
	require.Equal(t, "Renamed Standard", name)
	require.Equal(t, permsBefore, permsAfter)
	require.True(t, locked(otherID), "Purchaser must stay locked")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 124))
	for _, id := range ids {
		require.False(t, locked(id), "down must restore %s", id)
	}
	require.True(t, locked(otherID), "down must not unlock groups it did not lock")
}
