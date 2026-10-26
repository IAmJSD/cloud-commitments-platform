//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedGroupDownSlotTest is the UUID 000059 tried to seed Purchaser at. 000057
// had already given it to Standard Users, which is what it holds on every real
// database (issue #942).
const seedGroupDownSlotTest = "00000000-0000-5000-8000-000000000005"

// pinBeforeDown hands back a migrated database pinned at version, with the
// given users inserted, ready for a rollback of that one migration.
func pinBeforeDown(ctx context.Context, t *testing.T, version uint, users map[string][]string) *pgxpool.Pool {
	t.Helper()
	pool := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath()).DB.Pool()
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, getMigrationsPath(), version))
	for email, groups := range users {
		_, err := pool.Exec(ctx, `
			INSERT INTO users (id, email, password_hash, salt, active, group_ids, created_at, updated_at)
			VALUES (gen_random_uuid(), $1, '', '', true, $2::uuid[], NOW(), NOW())`, email, groups)
		require.NoError(t, err)
	}
	return pool
}

func groupName(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id string) (string, bool) {
	t.Helper()
	var name string
	err := pool.QueryRow(ctx, `SELECT name FROM groups WHERE id = $1`, id).Scan(&name)
	if err != nil {
		return "", false
	}
	return name, true
}

// makeSlotPurchaser models a database where 000059's seed did land at ...0005.
// Rolling back from head leaves 000064's Purchaser at ...0007 (its down is a
// no-op), so that row goes first to free the name.
func makeSlotPurchaser(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, `DELETE FROM groups WHERE name = 'Purchaser'`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE groups SET name = 'Purchaser' WHERE id = $1`, seedGroupDownSlotTest)
	require.NoError(t, err)
}

// TestMigration_SeedPurchaserGroup_Down covers issue #316 (A04-010). Rolling
// back 000059 deleted whatever sat at ...0005 and stripped it from every user,
// and at that point ...0005 is Standard Users, not Purchaser.
func TestMigration_SeedPurchaserGroup_Down(t *testing.T) {
	ctx := context.Background()
	below := previousMigrationVersion(t, 59)

	t.Run("Standard Users and its members survive the rollback", func(t *testing.T) {
		const standard, admin = "standard-059-down@test.example", "admin-059-down@test.example"
		pool := pinBeforeDown(ctx, t, 59, map[string][]string{
			standard: {seedGroupDownSlotTest},
			admin:    {adminGroupIDForPurchaserTest, seedGroupDownSlotTest},
		})
		name, ok := groupName(ctx, t, pool, seedGroupDownSlotTest)
		require.True(t, ok)
		require.Equal(t, "Standard Users", name, "precondition: 000057 owns ...0005")

		require.NoError(t, migrations.MigrateToVersion(ctx, pool, getMigrationsPath(), below),
			"rolling back 000059 must not trip users_min_one_group for a Standard-Users-only user")

		name, ok = groupName(ctx, t, pool, seedGroupDownSlotTest)
		assert.True(t, ok, "Standard Users must not be deleted by 000059's rollback")
		assert.Equal(t, "Standard Users", name)
		assert.ElementsMatch(t, []string{seedGroupDownSlotTest}, queryGroupIDsByEmail(t, ctx, pool, standard))
		assert.ElementsMatch(t, []string{adminGroupIDForPurchaserTest, seedGroupDownSlotTest}, queryGroupIDsByEmail(t, ctx, pool, admin))

		var hasColumn bool
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM information_schema.columns
			               WHERE table_name = 'groups' AND column_name = 'system_managed')`).Scan(&hasColumn))
		assert.False(t, hasColumn, "the rollback still drops system_managed")
	})

	// With no Standard-Users-only user nothing tripped the CHECK, so the old
	// rollback "succeeded" and silently destroyed the group.
	t.Run("Standard Users is not silently destroyed", func(t *testing.T) {
		const admin = "admin-059-silent@test.example"
		pool := pinBeforeDown(ctx, t, 59, map[string][]string{
			admin: {adminGroupIDForPurchaserTest, seedGroupDownSlotTest},
		})

		require.NoError(t, migrations.MigrateToVersion(ctx, pool, getMigrationsPath(), below))

		name, ok := groupName(ctx, t, pool, seedGroupDownSlotTest)
		assert.True(t, ok, "Standard Users must not be deleted by 000059's rollback")
		assert.Equal(t, "Standard Users", name)
		assert.ElementsMatch(t, []string{adminGroupIDForPurchaserTest, seedGroupDownSlotTest}, queryGroupIDsByEmail(t, ctx, pool, admin))
	})

	// Where the seed did land (...0005 really is Purchaser), the rollback still
	// removes it, and refuses rather than leave a Purchaser-only user with no
	// group.
	t.Run("a real Purchaser at ...0005 is still removed", func(t *testing.T) {
		const admin = "admin-059-purchaser@test.example"
		pool := pinBeforeDown(ctx, t, 59, map[string][]string{
			admin: {adminGroupIDForPurchaserTest, seedGroupDownSlotTest},
		})
		makeSlotPurchaser(ctx, t, pool)

		require.NoError(t, migrations.MigrateToVersion(ctx, pool, getMigrationsPath(), below))

		_, ok := groupName(ctx, t, pool, seedGroupDownSlotTest)
		assert.False(t, ok, "the Purchaser row 000059 seeded must be removed")
		assert.ElementsMatch(t, []string{adminGroupIDForPurchaserTest}, queryGroupIDsByEmail(t, ctx, pool, admin))
	})

	t.Run("a Purchaser-only user stops the rollback with a clear error", func(t *testing.T) {
		const only = "only-purchaser-059@test.example"
		pool := pinBeforeDown(ctx, t, 59, map[string][]string{only: {seedGroupDownSlotTest}})
		makeSlotPurchaser(ctx, t, pool)

		err := migrations.MigrateToVersion(ctx, pool, getMigrationsPath(), below)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "migration 000059 down")

		_, ok := groupName(ctx, t, pool, seedGroupDownSlotTest)
		assert.True(t, ok, "nothing is removed when the rollback refuses")
		assert.ElementsMatch(t, []string{seedGroupDownSlotTest}, queryGroupIDsByEmail(t, ctx, pool, only))
	})
}

// groupsSnapshot is every group row, in id order, as "id|name|system_managed".
func groupsSnapshot(ctx context.Context, t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT id::text || '|' || name || '|' || system_managed::text FROM groups ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var row string
		require.NoError(t, rows.Scan(&row))
		out = append(out, row)
	}
	require.NoError(t, rows.Err())
	return out
}

// TestMigration_SeedPurchaserGroup_DownUpRoundTrip covers the round trip
// tracked on issue #316: 000064's down is a no-op, so rolling back through
// 000059 leaves Purchaser at ...0007, and re-applying 000059 refused because
// a group named Purchaser already existed under an id other than ...0005.
// The round trip must succeed and leave the groups, including the relocated
// Purchaser's system_managed flag, and the users' memberships as they were:
// an admin who is not in Standard Users must not be added to it by 000059's
// admin backfill, which only ever meant Purchaser.
func TestMigration_SeedPurchaserGroup_DownUpRoundTrip(t *testing.T) {
	ctx := context.Background()

	t.Run("Standard Users at ...0005", func(t *testing.T) {
		const admin, standard = "admin-059-roundtrip@test.example", "standard-059-roundtrip@test.example"
		const lateAdmin = "late-admin-059-roundtrip@test.example"
		pool := pinBeforeDown(ctx, t, 64, map[string][]string{
			admin:     {adminGroupIDForPurchaserTest, seedGroupDownSlotTest, purchaserGroupIDTest},
			standard:  {seedGroupDownSlotTest},
			lateAdmin: {adminGroupIDForPurchaserTest, purchaserGroupIDTest},
		})
		assertSeedPurchaserRoundTrip(ctx, t, pool)
		assert.ElementsMatch(t, []string{adminGroupIDForPurchaserTest, seedGroupDownSlotTest, purchaserGroupIDTest}, queryGroupIDsByEmail(t, ctx, pool, admin))
		assert.ElementsMatch(t, []string{seedGroupDownSlotTest}, queryGroupIDsByEmail(t, ctx, pool, standard))
		assert.ElementsMatch(t, []string{adminGroupIDForPurchaserTest, purchaserGroupIDTest}, queryGroupIDsByEmail(t, ctx, pool, lateAdmin),
			"the re-applied backfill must not add an admin to Standard Users")
	})

	// With ...0005 free, the re-applied seed no longer hits its id conflict
	// and would insert a second "Purchaser", which groups.name's UNIQUE
	// constraint refuses.
	t.Run("...0005 free", func(t *testing.T) {
		const admin = "admin-059-roundtrip-free@test.example"
		pool := pinBeforeDown(ctx, t, 64, map[string][]string{
			admin: {adminGroupIDForPurchaserTest, purchaserGroupIDTest},
		})
		_, err := pool.Exec(ctx, `DELETE FROM groups WHERE id = $1`, seedGroupDownSlotTest)
		require.NoError(t, err)
		assertSeedPurchaserRoundTrip(ctx, t, pool)
		_, ok := groupName(ctx, t, pool, seedGroupDownSlotTest)
		assert.False(t, ok, "the re-applied seed must not create a group at ...0005")
		assert.ElementsMatch(t, []string{adminGroupIDForPurchaserTest, purchaserGroupIDTest}, queryGroupIDsByEmail(t, ctx, pool, admin))
	})
}

// The re-apply yields only to 000064's Purchaser at ...0007. A "Purchaser"
// under any other id is still someone else's group, and 000059 still refuses
// rather than silently skip its seed.
func TestMigration_SeedPurchaserGroup_ForeignPurchaserStillRefused(t *testing.T) {
	ctx := context.Background()
	pool := pinBeforeDown(ctx, t, 64, nil)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, getMigrationsPath(), previousMigrationVersion(t, 59)))
	_, err := pool.Exec(ctx, `DELETE FROM groups WHERE id = $1`, purchaserGroupIDTest)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO groups (id, name, description, permissions, allowed_accounts)
		VALUES ('00000000-0000-4000-8000-000000000316', 'Purchaser', 'operator-made', '[]'::jsonb, ARRAY['*'])`)
	require.NoError(t, err)

	err = migrations.MigrateToVersion(ctx, pool, getMigrationsPath(), 64)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "migration 000059: a group named 'Purchaser' already exists with a different id")
}

// assertSeedPurchaserRoundTrip rolls a database pinned at 000064 back through
// 000059 and forward again, and checks every group came back unchanged.
func assertSeedPurchaserRoundTrip(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	name, ok := groupName(ctx, t, pool, purchaserGroupIDTest)
	require.True(t, ok)
	require.Equal(t, "Purchaser", name, "precondition: 000064 put Purchaser at ...0007")
	groupsBefore := groupsSnapshot(ctx, t, pool)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, getMigrationsPath(), previousMigrationVersion(t, 59)),
		"rolling back through 000059 must succeed")
	name, ok = groupName(ctx, t, pool, purchaserGroupIDTest)
	require.True(t, ok, "precondition: 000064's no-op down leaves Purchaser at ...0007")
	require.Equal(t, "Purchaser", name)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, getMigrationsPath(), 64),
		"re-applying 000059 after its rollback must succeed")
	_, dirty, err := migrations.GetMigrationVersion(ctx, pool, getMigrationsPath())
	require.NoError(t, err)
	require.False(t, dirty)
	assert.Equal(t, groupsBefore, groupsSnapshot(ctx, t, pool), "the round trip must leave every group as it was")
}
