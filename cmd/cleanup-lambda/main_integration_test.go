//go:build integration

package main

// Real PostgreSQL tests for the cleanup job's purchase_executions predicate
// (issue #998). deleteExpired, dryRunCount and the migrated schema are real;
// nothing is deployed and no Lambda is invoked.

import (
	"context"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/require"
)

// Every status the purchase_executions CHECK allows is in exactly one of
// these lists, so a new status fails TestCleanupStatusListsCoverSchema until
// someone decides whether a lapsed TTL may delete it.
var (
	// Settled and not counted by the plan health score: a lapsed TTL deletes.
	cleanupDeletable = []string{"completed", "partially_completed", "expired", "revocation_requested"}
	// Live (an executor, scheduler or reaper may still act on it), or counted
	// by the health score and left to CleanupOldExecutions: always kept.
	cleanupKept = []string{"pending", "notified", "approved", "running", "scheduled", "paused", "failed", "canceled"}
)

func cleanupTestDB(ctx context.Context, t *testing.T) *database.Connection {
	t.Helper()
	c := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { _ = c.Cleanup(context.Background()) })
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "..", "..", "internal", "database", "postgres", "migrations")
	require.NoError(t, migrations.RunMigrations(ctx, c.DB.Pool(), path, "", ""))
	return c.DB
}

// insertExecution adds a plan-less execution whose expires_at is offset from
// the database clock (nil offset leaves it NULL) and returns its execution_id.
func insertExecution(ctx context.Context, t *testing.T, db *database.Connection, status string, offset *time.Duration) string {
	t.Helper()
	var secs *float64
	if offset != nil {
		s := offset.Seconds()
		secs = &s
	}
	var id string
	// NOW() + NULL is NULL, so a nil offset leaves expires_at unset.
	require.NoError(t, db.QueryRow(ctx, `INSERT INTO purchase_executions (status, scheduled_date, expires_at)
		VALUES ($1, NOW(), NOW() + make_interval(secs => $2::float8)) RETURNING execution_id::text`, status, secs).Scan(&id), "insert %s row", status)
	return id
}

func remainingExecutions(ctx context.Context, t *testing.T, db *database.Connection) map[string]string {
	t.Helper()
	rows, err := db.Query(ctx, `SELECT execution_id::text, status FROM purchase_executions`)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, status string
		require.NoError(t, rows.Scan(&id, &status))
		out[id] = status
	}
	require.NoError(t, rows.Err())
	return out
}

func TestCleanupStatusListsCoverSchema(t *testing.T) {
	ctx := context.Background()
	db := cleanupTestDB(ctx, t)
	var def string
	require.NoError(t, db.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = 'purchase_executions'::regclass AND contype = 'c'
		  AND pg_get_constraintdef(oid) LIKE '%status%'`).Scan(&def))
	matches := regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(def, -1)
	allowed := make([]string, 0, len(matches))
	for _, m := range matches {
		allowed = append(allowed, m[1])
	}
	classified := append(append([]string{}, cleanupDeletable...), cleanupKept...)
	sort.Strings(allowed)
	sort.Strings(classified)
	require.Equal(t, allowed, classified, "every allowed status must be classified exactly once")
}

// A lapsed TTL deletes only settled rows: live rows and the rows the health
// score counts survive, and the dry run counts exactly what the delete removes.
func TestDeleteExpiredKeepsLiveAndHealthScoredRows(t *testing.T) {
	ctx := context.Background()
	db := cleanupTestDB(ctx, t)
	lapsed, future := -time.Hour, time.Hour

	want := map[string]string{}
	for _, s := range cleanupKept {
		want[insertExecution(ctx, t, db, s, &lapsed)] = s
	}
	for _, s := range cleanupDeletable {
		insertExecution(ctx, t, db, s, &lapsed)
		want[insertExecution(ctx, t, db, s, &future)] = s
		want[insertExecution(ctx, t, db, s, nil)] = s
	}

	dry := &CleanupResult{}
	require.NoError(t, dryRunCount(ctx, db, time.Now(), dry))
	require.EqualValues(t, len(cleanupDeletable), dry.ExecutionsDeleted, "dry run counts only settled lapsed rows")

	got := &CleanupResult{}
	require.NoError(t, deleteExpired(ctx, db, time.Now(), got))
	require.EqualValues(t, len(cleanupDeletable), got.ExecutionsDeleted)
	require.Equal(t, want, remainingExecutions(ctx, t, db), "live, health-scored, unexpired and TTL-less rows survive")
}

// The DELETE rechecks the status on the committed row: a settled row a writer
// turns live while the DELETE waits for its lock survives. The writer is a
// stand-in (SavePurchaseExecution's upsert rewrites status unconditionally);
// no current flow moves a settled row back to live.
func TestDeleteExpiredRechecksRowAfterConcurrentWriter(t *testing.T) {
	for _, writerFirst := range []bool{true, false} {
		name := map[bool]string{true: "writer first", false: "cleanup first"}[writerFirst]
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := cleanupTestDB(ctx, t)
			lapsed := -time.Hour
			id := insertExecution(ctx, t, db, "completed", &lapsed)
			revive := `UPDATE purchase_executions SET status = 'pending' WHERE execution_id = $1`

			if !writerFirst {
				res := &CleanupResult{}
				require.NoError(t, deleteExpired(ctx, db, time.Now(), res))
				require.EqualValues(t, 1, res.ExecutionsDeleted)
				tag, err := db.Exec(ctx, revive, id)
				require.NoError(t, err)
				require.Zero(t, tag.RowsAffected(), "the settled row was already deleted")
				return
			}

			writer, err := db.Begin(ctx)
			require.NoError(t, err)
			t.Cleanup(func() { _ = writer.Rollback(context.Background()) })
			_, err = writer.Exec(ctx, revive, id)
			require.NoError(t, err)

			res := &CleanupResult{}
			done := make(chan error, 1)
			go func() { done <- deleteExpired(ctx, db, time.Now(), res) }()
			testhelpers.WaitForLockWait(ctx, t, db, "%DELETE FROM purchase_executions%")
			require.NoError(t, writer.Commit(ctx))
			require.NoError(t, testhelpers.Await(t, done))

			require.Zero(t, res.ExecutionsDeleted)
			require.Equal(t, map[string]string{id: "pending"}, remainingExecutions(ctx, t, db), "the row turned live survives")
		})
	}
}
