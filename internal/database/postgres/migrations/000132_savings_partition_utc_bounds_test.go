//go:build integration

package migrations_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

const utcPartitionBoundsVersion = 132

// execInZone runs sql in a transaction whose TimeZone is zone. SET LOCAL keeps
// the setting from leaking into the pooled connection.
func execInZone(ctx context.Context, t *testing.T, pool *pgxpool.Pool, zone, sql string, args ...any) error {
	t.Helper()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('TimeZone', $1, true)`, zone)
	require.NoError(t, err)
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func createPartitionInZone(ctx context.Context, t *testing.T, pool *pgxpool.Pool, zone, day string) {
	t.Helper()
	require.NoError(t, execInZone(ctx, t, pool, zone, `SELECT create_savings_snapshot_partition($1::date)`, day),
		"create partition for %s under TimeZone %s", day, zone)
}

// partitionBounds renders a partition's bounds in a UTC session.
func partitionBounds(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SET LOCAL TimeZone = 'UTC'; SET LOCAL DateStyle = 'ISO, YMD'`)
	require.NoError(t, err)
	var bound string
	require.NoError(t, tx.QueryRow(ctx,
		`SELECT pg_get_expr(relpartbound, oid) FROM pg_class WHERE relname = $1`, name).Scan(&bound),
		"partition %s must exist", name)
	return bound
}

// requireUTCMonthBounds asserts savings_snapshots_YYYY_MM spans exactly that
// UTC calendar month.
func requireUTCMonthBounds(ctx context.Context, t *testing.T, pool *pgxpool.Pool, year int, month time.Month) {
	t.Helper()
	start := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	want := fmt.Sprintf("FOR VALUES FROM ('%s 00:00:00+00') TO ('%s 00:00:00+00')",
		start.Format("2006-01-02"), end.Format("2006-01-02"))
	require.Equal(t, want, partitionBounds(ctx, t, pool, partitionName(start)))
}

// setDatabaseDefaults applies ALTER DATABASE <current> <clause> for each
// clause; only sessions opened afterwards (such as the migrator's) see it.
func setDatabaseDefaults(ctx context.Context, t *testing.T, pool *pgxpool.Pool, clauses ...string) {
	t.Helper()
	var db string
	require.NoError(t, pool.QueryRow(ctx, `SELECT quote_ident(current_database())`).Scan(&db))
	for _, c := range clauses {
		_, err := pool.Exec(ctx, "ALTER DATABASE "+db+" "+c)
		require.NoError(t, err)
	}
}

func partitionName(ts time.Time) string {
	return "savings_snapshots_" + ts.UTC().Format("2006_01")
}

func insertSnapshot(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id string, ts time.Time) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO savings_snapshots (id, account_id, timestamp, provider, service, region, commitment_type)
		VALUES ($1, 'acct-912', $2, 'aws', 'ec2', 'us-east-1', 'RI')`, id, ts)
	require.NoError(t, err)
}

func snapshotPartition(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var part string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT tableoid::regclass::text FROM savings_snapshots WHERE id = $1`, id).Scan(&part))
	return part
}

func relOID(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) uint32 {
	t.Helper()
	var oid uint32
	require.NoError(t, pool.QueryRow(ctx, `SELECT oid FROM pg_class WHERE relname = $1`, name).Scan(&oid))
	return oid
}

func utc(s string) time.Time {
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return ts.UTC()
}

// TestMigration_SavingsPartitionUTCBounds covers 000132 (#912): partitions
// created under any session TimeZone span UTC months, and partitions that the
// old function cut at local midnight are rebuilt with their rows re-routed.
func TestMigration_SavingsPartitionUTCBounds(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { require.NoError(t, container.Cleanup(context.Background())) })
	pool, path := container.DB.Pool(), getMigrationsPath()
	prev := previousMigrationVersion(t, utcPartitionBoundsVersion)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))

	// Old function, mixed session zones: Berlin cuts December at 23:00Z the day
	// before, UTC cuts January correctly, Los Angeles cuts March at 08:00Z and
	// April at 07:00Z (DST).
	createPartitionInZone(ctx, t, pool, "Europe/Berlin", "2031-12-15")
	createPartitionInZone(ctx, t, pool, "UTC", "2032-01-15")
	createPartitionInZone(ctx, t, pool, "America/Los_Angeles", "2032-03-15")
	alignedJanOID := relOID(ctx, t, pool, "savings_snapshots_2032_01")

	rows := map[string]time.Time{
		"00000000-0000-0000-0000-000000000912": utc("2031-11-30T23:30:00Z"), // UTC November, in Berlin's December
		"00000000-0000-0000-0000-000000000913": utc("2031-12-15T12:00:00Z"),
		"00000000-0000-0000-0000-000000000914": utc("2031-12-31T23:30:00Z"), // gap between Berlin Dec and UTC Jan
		"00000000-0000-0000-0000-000000000915": utc("2032-01-10T00:00:00Z"),
		"00000000-0000-0000-0000-000000000916": utc("2032-04-01T03:00:00Z"), // UTC April, in LA's March
	}
	for id, ts := range rows {
		insertSnapshot(ctx, t, pool, id, ts)
	}
	// The bug, before 000132.
	require.Equal(t, "savings_snapshots_2031_12", snapshotPartition(ctx, t, pool, "00000000-0000-0000-0000-000000000912"))
	require.Equal(t, "savings_snapshots_default", snapshotPartition(ctx, t, pool, "00000000-0000-0000-0000-000000000914"))
	require.Equal(t, "savings_snapshots_2032_03", snapshotPartition(ctx, t, pool, "00000000-0000-0000-0000-000000000916"))

	// Run the repair from a session that is neither UTC nor ISO-formatted
	// (SQL DateStyle renders "IST", an ambiguous abbreviation), so neither the
	// new literals nor the aligned-bounds comparison can lean on the defaults.
	setDatabaseDefaults(ctx, t, pool, `SET TimeZone = 'Asia/Kolkata'`, `SET DateStyle = 'SQL, DMY'`)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, utcPartitionBoundsVersion))
	setDatabaseDefaults(ctx, t, pool, `RESET TimeZone`, `RESET DateStyle`)

	for _, m := range []time.Time{
		utc("2031-11-01T00:00:00Z"), utc("2031-12-01T00:00:00Z"), utc("2032-01-01T00:00:00Z"),
		utc("2032-03-01T00:00:00Z"), utc("2032-04-01T00:00:00Z"),
	} {
		requireUTCMonthBounds(ctx, t, pool, m.Year(), m.Month())
	}
	for id, ts := range rows {
		require.Equal(t, partitionName(ts), snapshotPartition(ctx, t, pool, id), "row %s at %s", id, ts)
	}
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM savings_snapshots WHERE account_id = 'acct-912'`).Scan(&n))
	require.Equal(t, len(rows), n, "the rebuild must keep every row exactly once")
	require.Equal(t, alignedJanOID, relOID(ctx, t, pool, "savings_snapshots_2032_01"),
		"an already-aligned partition must not be rebuilt")

	t.Run("function cuts UTC months under any session TimeZone", func(t *testing.T) {
		createPartitionInZone(ctx, t, pool, "Europe/Berlin", "2033-12-15")
		createPartitionInZone(ctx, t, pool, "America/Los_Angeles", "2034-01-15")
		requireUTCMonthBounds(ctx, t, pool, 2033, time.December)
		requireUTCMonthBounds(ctx, t, pool, 2034, time.January)

		insertSnapshot(ctx, t, pool, "00000000-0000-0000-0000-000000000917", utc("2033-12-31T23:30:00Z"))
		require.Equal(t, "savings_snapshots_2033_12", snapshotPartition(ctx, t, pool, "00000000-0000-0000-0000-000000000917"))

		// A Berlin session following a UTC neighbor used to fail with "would overlap".
		createPartitionInZone(ctx, t, pool, "UTC", "2034-05-15")
		createPartitionInZone(ctx, t, pool, "Europe/Berlin", "2034-06-15")
		requireUTCMonthBounds(ctx, t, pool, 2034, time.June)
	})

	t.Run("future partitions start at the current UTC month", func(t *testing.T) {
		// UTC+14: its local date runs ahead of UTC.
		require.NoError(t, execInZone(ctx, t, pool, "Pacific/Kiritimati", `SELECT create_future_savings_partitions(1)`))
		now := time.Now().UTC()
		requireUTCMonthBounds(ctx, t, pool, now.Year(), now.Month())
		next := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
		requireUTCMonthBounds(ctx, t, pool, next.Year(), next.Month())
	})

	t.Run("down restores the old function and up repairs again", func(t *testing.T) {
		require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))
		createPartitionInZone(ctx, t, pool, "Europe/Berlin", "2035-12-15")
		require.Contains(t, partitionBounds(ctx, t, pool, "savings_snapshots_2035_12"), "2035-11-30 23:00:00+00",
			"down must restore the 000003 session-zone behavior")

		require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, utcPartitionBoundsVersion))
		requireUTCMonthBounds(ctx, t, pool, 2035, time.December)
	})
}

// TestMigration_SavingsPartitionConcurrentCreate checks that a second caller
// for the same month, arriving while the first has created the partition but
// not committed, waits and then succeeds instead of failing with "relation
// already exists".
func TestMigration_SavingsPartitionConcurrentCreate(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { require.NoError(t, container.Cleanup(context.Background())) })
	pool := container.DB.Pool()
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, getMigrationsPath(), utcPartitionBoundsVersion))

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT create_savings_snapshot_partition('2036-02-15')`)
	require.NoError(t, err)

	second, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer second.Release()
	secondPID := second.Conn().PgConn().PID()

	done := make(chan error, 1)
	go func() {
		_, err := second.Exec(ctx, `SELECT create_savings_snapshot_partition('2036-02-15')`)
		done <- err
	}()

	// Commit only once the second caller is blocked on a lock, so it really
	// overlaps the first one's uncommitted CREATE.
	require.Eventually(t, func() bool {
		var waiting bool
		err := pool.QueryRow(ctx, `
			SELECT COALESCE(wait_event_type = 'Lock', false)
			FROM pg_stat_activity WHERE pid = $1`, secondPID).Scan(&waiting)
		return err == nil && waiting
	}, 10*time.Second, 20*time.Millisecond, "second caller never blocked on the first")
	require.NoError(t, tx.Commit(ctx))

	select {
	case err := <-done:
		require.NoError(t, err, "a concurrent caller for the same month must not fail")
	case <-time.After(10 * time.Second):
		t.Fatal("second caller did not finish after the first committed")
	}
	requireUTCMonthBounds(ctx, t, pool, 2036, time.February)
}
