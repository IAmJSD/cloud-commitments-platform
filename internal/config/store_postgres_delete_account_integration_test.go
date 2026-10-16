//go:build integration

package config

// Real PostgreSQL tests for DeleteCloudAccount against purchase_executions
// history (issue #265). Migration 000053 made the execution FK ON DELETE
// RESTRICT, which used to block the delete for every referencing row
// whatever its status, so one failed execution made an account undeletable.

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedDeleteAccount(ctx context.Context, t *testing.T, store *PostgresStore) string {
	t.Helper()
	id := uuid.New().String()
	_, err := store.db.Exec(ctx, `
		INSERT INTO cloud_accounts (id, name, enabled, provider, external_id, aws_is_org_root, created_at, updated_at)
		VALUES ($1, 'Delete Test Account', true, 'azure', $2, false, now(), now())`, id, id)
	require.NoError(t, err, "seed cloud_accounts")
	return id
}

// liveExecutionStatuses are the schema's statuses outside
// TerminalExecutionStatuses. TestTerminalExecutionStatuses_CoverSchema keeps
// the two lists equal to the status CHECK constraint between them.
var liveExecutionStatuses = []string{"pending", "notified", "approved", "running", "scheduled", "paused"}

// seedDeleteAccountExec inserts an execution in status on accountID with an
// old updated_at, so a later bump is visible. It is a plain INSERT on purpose:
// the BEFORE UPDATE trigger would overwrite an updated_at set by an UPDATE.
func seedDeleteAccountExec(ctx context.Context, t *testing.T, store *PostgresStore, accountID, status string) string {
	t.Helper()
	var id string
	require.NoError(t, store.db.QueryRow(ctx, `
		INSERT INTO purchase_executions (status, step_number, scheduled_date, cloud_account_id, updated_at)
		VALUES ($1, 1, NOW(), $2, $3)
		RETURNING execution_id::text`, status, accountID, agedUpdatedAt).Scan(&id),
		"seed execution in status %s", status)
	return id
}

var agedUpdatedAt = time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)

func storedUpdatedAt(ctx context.Context, t *testing.T, store *PostgresStore, execID string) time.Time {
	t.Helper()
	var ts time.Time
	require.NoError(t, store.db.QueryRow(ctx,
		`SELECT updated_at FROM purchase_executions WHERE execution_id = $1`, execID).Scan(&ts))
	return ts.UTC()
}

func storedExecAccount(ctx context.Context, t *testing.T, store *PostgresStore, execID string) (status string, accountID *string) {
	t.Helper()
	require.NoError(t, store.db.QueryRow(ctx,
		`SELECT status, cloud_account_id::text FROM purchase_executions WHERE execution_id = $1`, execID,
	).Scan(&status, &accountID), "execution %s must survive the account delete", execID)
	return status, accountID
}

func accountExists(ctx context.Context, t *testing.T, store *PostgresStore, id string) bool {
	t.Helper()
	var n int
	require.NoError(t, store.db.QueryRow(ctx, `SELECT COUNT(*) FROM cloud_accounts WHERE id = $1`, id).Scan(&n))
	return n == 1
}

// An account whose executions are all terminal is deletable, and the history
// rows survive with their status intact and the account reference detached.
func TestDeleteCloudAccount_TerminalExecutionsAreDetached(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)

	for _, status := range TerminalExecutionStatuses {
		t.Run(status, func(t *testing.T) {
			acct := seedDeleteAccount(ctx, t, store)
			execID := seedDeleteAccountExec(ctx, t, store, acct, status)

			require.NoError(t, store.DeleteCloudAccount(ctx, acct))

			assert.False(t, accountExists(ctx, t, store, acct), "account row must be gone")
			gotStatus, gotAcct := storedExecAccount(ctx, t, store, execID)
			assert.Equal(t, status, gotStatus, "history row status must be unchanged")
			assert.Nil(t, gotAcct, "history row must be detached from the deleted account")
			assert.True(t, storedUpdatedAt(ctx, t, store, execID).Equal(agedUpdatedAt),
				"detaching must not move updated_at (plan health and retention read it as a lifecycle clock)")
		})
	}
}

// A live execution still blocks the delete at the FK, and the transaction
// rolls back whole: the account survives and its terminal history stays
// attached to it.
func TestDeleteCloudAccount_LiveExecutionBlocks(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)

	for _, status := range liveExecutionStatuses {
		t.Run(status, func(t *testing.T) {
			acct := seedDeleteAccount(ctx, t, store)
			liveID := seedDeleteAccountExec(ctx, t, store, acct, status)
			historyID := seedDeleteAccountExec(ctx, t, store, acct, StatusFailed)

			n, err := store.CountLiveExecutionsForAccount(ctx, acct)
			require.NoError(t, err)
			assert.Equal(t, 1, n, "only the %s row is live", status)

			err = store.DeleteCloudAccount(ctx, acct)
			require.Error(t, err)
			var pgErr *pgconn.PgError
			require.True(t, errors.As(err, &pgErr), "want a *pgconn.PgError, got %v", err)
			assert.Equal(t, "23503", pgErr.Code, "the RESTRICT FK must still refuse a live row")

			assert.True(t, accountExists(ctx, t, store, acct), "account must survive a refused delete")
			for _, id := range []string{liveID, historyID} {
				_, gotAcct := storedExecAccount(ctx, t, store, id)
				require.NotNil(t, gotAcct, "a refused delete must leave %s attached", id)
				assert.Equal(t, acct, *gotAcct)
			}
		})
	}
}

// Detaching is scoped to the deleted account: another account's terminal
// execution keeps its reference.
func TestDeleteCloudAccount_DetachIsScopedToAccount(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	gone, kept := seedDeleteAccount(ctx, t, store), seedDeleteAccount(ctx, t, store)
	seedDeleteAccountExec(ctx, t, store, gone, StatusCompleted)
	keptID := seedDeleteAccountExec(ctx, t, store, kept, StatusCompleted)

	require.NoError(t, store.DeleteCloudAccount(ctx, gone))

	_, gotAcct := storedExecAccount(ctx, t, store, keptID)
	require.NotNil(t, gotAcct)
	assert.Equal(t, kept, *gotAcct)
}

// CountLiveExecutionsForAccount counts every non-terminal row of the account
// and nothing else; CountPendingExecutionsForAccount keeps counting only the
// cancelable pending/notified subset.
func TestCountLiveExecutionsForAccount(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	acct, other := seedDeleteAccount(ctx, t, store), seedDeleteAccount(ctx, t, store)
	for _, status := range liveExecutionStatuses {
		seedDeleteAccountExec(ctx, t, store, acct, status)
	}
	for _, status := range TerminalExecutionStatuses {
		seedDeleteAccountExec(ctx, t, store, acct, status)
	}
	seedDeleteAccountExec(ctx, t, store, other, "running")

	live, err := store.CountLiveExecutionsForAccount(ctx, acct)
	require.NoError(t, err)
	assert.Equal(t, len(liveExecutionStatuses), live)

	pending, err := store.CountPendingExecutionsForAccount(ctx, acct)
	require.NoError(t, err)
	assert.Equal(t, 2, pending, "pending + notified")
}

// TerminalExecutionStatuses plus liveExecutionStatuses must be exactly the
// statuses the schema accepts. A status added to the CHECK constraint fails
// this test until it is classified: as terminal (detached on account delete)
// or live (blocks the delete, the default the allowlist gives it).
func TestTerminalExecutionStatuses_CoverSchema(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)

	var def string
	require.NoError(t, store.db.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint
		 WHERE conname = 'purchase_executions_status_check'`).Scan(&def))
	matches := regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(def, -1)
	schema := make([]string, 0, len(matches))
	for _, m := range matches {
		schema = append(schema, m[1])
	}
	require.NotEmpty(t, schema, "could not parse %q", def)

	classified := append(append([]string{}, TerminalExecutionStatuses...), liveExecutionStatuses...)
	sort.Strings(schema)
	sort.Strings(classified)
	assert.Equal(t, schema, classified)
}
