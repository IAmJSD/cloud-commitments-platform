//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigration_SavingsSnapshotsPK verifies that migration 000027 is
// idempotent on a fresh database.
//
// On a fresh DB, migration 000018 already adds savings_snapshots_pkey. Before
// the fix, 000027's bare ADD CONSTRAINT would fail with "multiple primary
// keys for table savings_snapshots". After the fix, the DROP CONSTRAINT IF
// EXISTS guard makes 000027 safe to apply regardless of whether 000018 has
// already created the constraint.
func TestMigration_SavingsSnapshotsPK(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	migrationsPath := getMigrationsPath()

	t.Run("full migration from scratch succeeds (000018 + 000027 coexist)", func(t *testing.T) {
		t.Parallel()
		container := testhelpers.RequireEmptyDatabase(ctx, t)
		pool := container.DB.Pool()

		// Running all migrations must succeed on a fresh DB — 000027's
		// DROP CONSTRAINT IF EXISTS prevents "multiple primary keys"
		// that the pre-fix ADD CONSTRAINT caused.
		require.NoError(t, migrations.RunMigrations(ctx, pool, migrationsPath, "", ""),
			"full migration from scratch should succeed with idempotent 000027")

		// The primary key must exist and cover (id, timestamp).
		var constraintExists bool
		err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_constraint
				WHERE conname = 'savings_snapshots_pkey'
				  AND conrelid = 'savings_snapshots'::regclass
				  AND contype = 'p'
			)`).Scan(&constraintExists)
		require.NoError(t, err)
		assert.True(t, constraintExists, "savings_snapshots_pkey should exist after migrations")
	})

	t.Run("rollback to 000026 then re-apply 000027 succeeds", func(t *testing.T) {
		t.Parallel()
		container := testhelpers.RequireEmptyDatabase(ctx, t)
		pool := container.DB.Pool()

		// Pin the schema at 000027 (which re-created the PK over 000018's
		// version), then roll back exactly 000027. The previous
		// count-rows-in-schema_migrations approach was broken: golang-migrate
		// keeps a single (version, dirty) row, so the count was always 1 and
		// the loop only ever rolled back the newest migration.
		require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 27))
		require.NoError(t, migrations.RollbackMigrations(ctx, pool, migrationsPath, 1),
			"rollback of 000027 should succeed")

		// Verify the constraint is gone (000027 was rolled back).
		var constraintExists bool
		err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_constraint
				WHERE conname = 'savings_snapshots_pkey'
				  AND conrelid = 'savings_snapshots'::regclass
				  AND contype = 'p'
			)`).Scan(&constraintExists)
		require.NoError(t, err)
		assert.False(t, constraintExists, "savings_snapshots_pkey should be gone after rollback past 000027")

		// Re-apply all migrations — 000027 must succeed even though
		// 000018's schema_migrations entry is still present (that migration
		// was not rolled back, though its original constraint was superseded
		// when 000027 ran and dropped/re-created savings_snapshots_pkey).
		require.NoError(t, migrations.RunMigrations(ctx, pool, migrationsPath, "", ""),
			"re-applying 000027 over a DB where 000018 already ran must succeed")

		err = pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_constraint
				WHERE conname = 'savings_snapshots_pkey'
				  AND conrelid = 'savings_snapshots'::regclass
				  AND contype = 'p'
			)`).Scan(&constraintExists)
		require.NoError(t, err)
		assert.True(t, constraintExists, "savings_snapshots_pkey should be restored after re-apply")
	})
}

// TestMigration_SavingsSnapshotsPK_DedupeStaysInPartition covers issue #316
// (A04-011). savings_snapshots is partitioned by month and ctid is a position
// within one partition's heap, so the first row of every partition sits at
// (0,1). The dedupe joined on ctid alone: deleting a duplicate at (0,1) in one
// partition also deleted the unrelated row at (0,1) of every other partition.
// The migration's own header anticipates a forced re-run, which is when that
// would hit a database that has accumulated a duplicate.
func TestMigration_SavingsSnapshotsPK_DedupeStaysInPartition(t *testing.T) {
	ctx := context.Background()
	pool := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath()).DB.Pool()

	const (
		dupID       = "00000000-0000-0000-0000-000000316001"
		unrelatedID = "00000000-0000-0000-0000-000000316002"
	)
	jan := time.Date(2041, 1, 15, 0, 0, 0, 0, time.UTC)
	feb := time.Date(2041, 2, 15, 0, 0, 0, 0, time.UTC)
	_, err := pool.Exec(ctx, `SELECT create_savings_snapshot_partition('2041-01-15'), create_savings_snapshot_partition('2041-02-15')`)
	require.NoError(t, err)

	// The pre-000027 state: no primary key, so duplicates can exist.
	_, err = pool.Exec(ctx, `ALTER TABLE savings_snapshots DROP CONSTRAINT savings_snapshots_pkey`)
	require.NoError(t, err)

	insert := func(id string, ts, createdAt time.Time) {
		t.Helper()
		_, insertErr := pool.Exec(ctx, `
			INSERT INTO savings_snapshots (id, account_id, timestamp, provider, service, region, commitment_type, created_at)
			VALUES ($1, 'acct-316', $2, 'aws', 'ec2', 'us-east-1', 'RI', $3)`, id, ts, createdAt)
		require.NoError(t, insertErr)
	}
	now := time.Now().UTC()
	insert(dupID, jan, now.Add(-time.Hour)) // the older duplicate: deleted
	insert(dupID, jan, now)                 // the newer duplicate: kept
	insert(unrelatedID, feb, now.Add(-time.Hour))

	ctidOf := func(where string, args ...any) string {
		t.Helper()
		var ctid string
		require.NoError(t, pool.QueryRow(ctx, `SELECT ctid::text FROM savings_snapshots WHERE `+where, args...).Scan(&ctid))
		return ctid
	}
	require.Equal(t, ctidOf(`id = $1 AND created_at < $2`, dupID, now), ctidOf(`id = $1`, unrelatedID),
		"precondition: the deleted duplicate and the unrelated row share a ctid in different partitions")

	upSQL, err := os.ReadFile(filepath.Join(getMigrationsPath(), "000027_savings_snapshots_pk.up.sql"))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(upSQL))
	require.NoError(t, err, "re-running 000027 must succeed")

	var unrelated int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM savings_snapshots WHERE id = $1`, unrelatedID).Scan(&unrelated))
	assert.Equal(t, 1, unrelated, "a row in another partition must not be deleted by the dedupe")

	var kept int
	var keptCreated time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*), max(created_at) FROM savings_snapshots WHERE id = $1`, dupID).Scan(&kept, &keptCreated))
	assert.Equal(t, 1, kept, "exactly one copy of the duplicate must remain")
	assert.WithinDuration(t, now, keptCreated, time.Millisecond, "the newest copy is the one kept")
}
