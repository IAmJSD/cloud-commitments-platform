//go:build integration

package config

import (
	"context"
	"regexp"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// cleanupTTLDeletable are the statuses CleanupOldExecutions' expires_at branch
// may delete: settled (nothing acts on them again) and not counted by the plan
// health score. Spelled out literally rather than derived from
// TerminalExecutionStatuses, so a change to either shared list has to be made
// here too, on purpose.
var cleanupTTLDeletable = []string{StatusCompleted, StatusPartiallyCompleted, StatusExpired, StatusRevocationRequested}

// executionStatusesAllowedBySchema reads the purchase_executions status CHECK,
// so a status added to the schema is covered by the test below without anyone
// having to remember it.
func executionStatusesAllowedBySchema(ctx context.Context, t *testing.T, store *PostgresStore) []string {
	t.Helper()
	var def string
	require.NoError(t, store.db.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = 'purchase_executions'::regclass AND contype = 'c'
		  AND pg_get_constraintdef(oid) LIKE '%status%'`).Scan(&def))
	matches := regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(def, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	require.NotEmpty(t, out, "parse the status CHECK: %s", def)
	return out
}

// insertCleanupRow adds a plan-less execution whose expires_at is expiresDays
// from the database clock (nil leaves it NULL). scheduled_date is far in the
// future and updated_at is now, so only the expires_at branch can match it.
func insertCleanupRow(ctx context.Context, t *testing.T, store *PostgresStore, status string, expiresDays *int) string {
	t.Helper()
	var id string
	require.NoError(t, store.db.QueryRow(ctx, `
		INSERT INTO purchase_executions (status, scheduled_date, updated_at, expires_at)
		VALUES ($1, NOW() + INTERVAL '400 days', NOW(), NOW() + make_interval(days => $2::int))
		RETURNING execution_id::text`, status, expiresDays).Scan(&id), "insert %s row", status)
	return id
}

// TestPostgresStoreDB_CleanupOldExecutions_ExpiryBranchDeletesOnlySettledRows
// is the regression guard for #120: the expires_at branch used to delete a
// row in any status the health score does not count, so an approved or
// running row stranded by a dead executor, with a lapsed TTL, was deleted
// before GetStaleApprovedExecutions or ListStuckExecutions could resolve it.
// Every status the schema allows gets a lapsed-TTL row; only the settled,
// unscored ones may go. Fail-before: approved, running, pending, notified,
// scheduled and paused rows are deleted.
func TestPostgresStoreDB_CleanupOldExecutions_ExpiryBranchDeletesOnlySettledRows(t *testing.T) {
	conn := setupTestContainerDB(t)
	cleanupTestData(t, conn)
	store := NewPostgresStore(conn)
	ctx := context.Background()

	allowed := executionStatusesAllowedBySchema(ctx, t, store)
	for _, s := range append([]string{"approved", "running"}, cleanupTTLDeletable...) {
		require.Contains(t, allowed, s, "the schema must still allow %s for this test to mean anything", s)
	}

	lapsed, inside := -40, -10
	want := map[string]string{}
	for _, s := range allowed {
		id := insertCleanupRow(ctx, t, store, s, &lapsed)
		if !slices.Contains(cleanupTTLDeletable, s) {
			want[id] = s
			continue
		}
		// A deletable status still survives with a TTL inside retention or none.
		want[insertCleanupRow(ctx, t, store, s, &inside)] = s
		want[insertCleanupRow(ctx, t, store, s, nil)] = s
	}

	deleted, err := store.CleanupOldExecutions(ctx, 30)
	require.NoError(t, err)
	require.EqualValues(t, len(cleanupTTLDeletable), deleted, "only one lapsed row per settled, unscored status goes")

	rows, err := store.db.Query(ctx, `SELECT execution_id::text, status FROM purchase_executions`)
	require.NoError(t, err)
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var id, status string
		require.NoError(t, rows.Scan(&id, &status))
		got[id] = status
	}
	require.NoError(t, rows.Err())
	require.Equal(t, want, got, "live, health-scored, unexpired and TTL-less rows survive")
}
