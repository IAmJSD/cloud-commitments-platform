package testhelpers

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// LockDB is the slice of a pool the lock helpers need. The config store's
// dbConn and a *pgxpool.Pool both satisfy it.
type LockDB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Footprint stands in for the editor command's lock footprint on one plan so
// the race tests of legacy writers run before the command exists: the plan row
// FOR UPDATE, then its executions ordered FOR UPDATE, then its accounts FOR
// SHARE. It does NOT take the command's first two lock levels, the identity
// advisory lock and the sorted provider advisory locks (LockPlanProvidersTx).
// The embedded Tx is the command's transaction: Exec applies the command's
// writes (SetManaged, inserted roots) and Commit/Rollback end it. Executions
// lists the rows the footprint locked, i.e. what the command captured: a
// writer that committed before the footprint must appear here.
//
// Swap contract for the ExecutePlanScheduleCommand race tests: replace
// HoldCommandFootprint with the real command paused after its capture, wrapped
// as a *Footprint (Tx = the command's transaction, Executions = its captured
// set). Then (1) the notifier claim queues first on the provider advisory lock,
// so its WaitForLockWait pattern changes from "%FOR UPDATE%" to the advisory
// wait (wait_event 'advisory'; match the pg_advisory_xact_lock statement);
// (2) the direct Exec/QueryRow calls of the retry, prune and account-delete
// tests become the command's own writes and reads; every other assertion stays.
type Footprint struct {
	pgx.Tx
	Executions []string
}

// HoldCommandFootprint opens the footprint on planID. The transaction rolls
// back at test end unless the test ended it first.
func HoldCommandFootprint(ctx context.Context, t *testing.T, db LockDB, planID string) *Footprint {
	t.Helper()
	tx, err := db.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			t.Logf("footprint rollback: %v", rbErr)
		}
	})
	f := &Footprint{Tx: tx}
	_, err = tx.Exec(ctx, `SELECT 1 FROM purchase_plans WHERE id = $1 FOR UPDATE`, planID)
	require.NoError(t, err)
	rows, err := tx.Query(ctx, `SELECT execution_id::text FROM purchase_executions WHERE plan_id = $1 ORDER BY execution_id FOR UPDATE`, planID)
	require.NoError(t, err)
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		f.Executions = append(f.Executions, id)
	}
	require.NoError(t, rows.Err())
	_, err = tx.Exec(ctx, `SELECT 1 FROM cloud_accounts WHERE id IN (SELECT account_id FROM plan_accounts WHERE plan_id = $1) ORDER BY id FOR SHARE`, planID)
	require.NoError(t, err)
	return f
}

// SetManaged writes the editor marker inside the footprint's transaction.
func (f *Footprint) SetManaged(ctx context.Context, t *testing.T, planID string) {
	t.Helper()
	_, err := f.Exec(ctx, `UPDATE purchase_plans SET ramp_schedule = ramp_schedule || '{"dates_managed":true}' WHERE id = $1`, planID)
	require.NoError(t, err)
}

// WaitForLockWait blocks until some backend is waiting on a lock in a
// statement matching queryLike (a SQL LIKE pattern; "" matches any), which
// proves the writer under test is queued behind the footprint.
func WaitForLockWait(ctx context.Context, t *testing.T, db LockDB, queryLike string) {
	t.Helper()
	if queryLike == "" {
		queryLike = "%"
	}
	require.Eventually(t, func() bool {
		var n int
		err := db.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()
			AND wait_event_type = 'Lock' AND query LIKE $1`, queryLike).Scan(&n)
		return err == nil && n > 0
	}, 15*time.Second, 20*time.Millisecond, "writer must wait on the footprint lock")
}

// Await returns the value a background writer sends on done, failing fast
// instead of hanging until the go test timeout when the writer is stuck.
func Await[T any](t *testing.T, done <-chan T) T {
	t.Helper()
	select {
	case v := <-done:
		return v
	case <-time.After(30 * time.Second):
		t.Fatal("writer did not finish")
		panic("unreachable")
	}
}

// lockPollInterval and lockPollTimeout bound the lock observations below. The
// timeout only fails a stuck test; a passing run returns as soon as the
// observed state appears.
const (
	lockPollInterval = 5 * time.Millisecond
	lockPollTimeout  = 30 * time.Second
)

// BackendPID returns the server process ID of tx's session, the holderPID that
// AwaitBlockedBy takes.
func BackendPID(ctx context.Context, t *testing.T, tx pgx.Tx) int {
	t.Helper()
	var pid int
	require.NoError(t, tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	return pid
}

// AwaitBlockedBy proves a contender is queued behind holderPID's transaction:
// it returns once some backend waits on a lock that holderPID holds, so the
// contender cannot finish before the holder ends. If done delivers first, the
// contender answered without contending; AwaitBlockedBy returns that value and
// true. A fixed quiet window (select on done against time.After) cannot tell
// "blocked" from "not there yet", so on a slow runner it passes a contender
// that never contends. This observation cannot.
//
// Query db from a connection other than the holder's: pg_locks is read live,
// but the holder's session is busy holding the lock.
func AwaitBlockedBy[T any](ctx context.Context, t *testing.T, db LockDB, holderPID int, done <-chan T) (T, bool) {
	t.Helper()
	deadline := time.Now().Add(lockPollTimeout)
	for time.Now().Before(deadline) {
		var blocked bool
		require.NoError(t, db.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_locks WHERE NOT granted AND $1::int = ANY(pg_blocking_pids(pid)))`, holderPID).Scan(&blocked))
		if blocked {
			var zero T
			return zero, false
		}
		select {
		case v := <-done:
			return v, true
		case <-time.After(lockPollInterval):
		}
	}
	t.Fatalf("no backend blocked on backend %d within %s and the contender did not return", holderPID, lockPollTimeout)
	panic("unreachable")
}

// LockWaiters counts backends of this database that wait on a lock in a
// statement matching queryLike (a SQL LIKE pattern).
func LockWaiters(ctx context.Context, db LockDB, queryLike string) (int, error) {
	var n int
	err := db.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()
		AND wait_event_type = 'Lock' AND query LIKE $1`, queryLike).Scan(&n)
	return n, err
}

// AwaitArrivalsOrLockWaits is a race-test barrier: it returns once each of
// parties has either arrived (arrived reports how many have) or is waiting on
// a lock in a statement matching queryLike. Under a serializing lock the
// parties that have not arrived are provably parked on it, so waiting longer
// cannot change the outcome; without the lock every party arrives. Either way
// the barrier opens on observed state, never on a guessed timeout. It returns
// an error rather than failing a test so the goroutine under test can call it.
func AwaitArrivalsOrLockWaits(ctx context.Context, db LockDB, parties int, arrived func() int, queryLike string) error {
	deadline := time.Now().Add(lockPollTimeout)
	for time.Now().Before(deadline) {
		// Read arrivals first: a party that arrives between the two reads is
		// then missed for one round instead of counted twice.
		n := arrived()
		waiting, err := LockWaiters(ctx, db, queryLike)
		if err != nil {
			return err
		}
		if n+waiting >= parties {
			return nil
		}
		time.Sleep(lockPollInterval)
	}
	return fmt.Errorf("barrier: %d of %d parties neither arrived nor waited on a %q lock within %s",
		parties-arrived(), parties, queryLike, lockPollTimeout)
}
