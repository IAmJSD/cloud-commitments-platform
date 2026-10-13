//go:build integration

package config

// Real PostgreSQL tests for the status-guarded execution saves (issue #829).

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func storedStatus(ctx context.Context, t *testing.T, store *PostgresStore, id string) string {
	t.Helper()
	var v string
	require.NoError(t, store.db.QueryRow(ctx, `SELECT status FROM purchase_executions WHERE execution_id = $1`, id).Scan(&v))
	return v
}

func TestSaveExecutionIfStatus_WritesOnlyWhenStatusMatches(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	exec := newNoSubmissionExec(ctx, t, store, "running")

	// Stale copy of the pre-claim row, as the approve path holds it.
	stale := *exec
	stale.Status = "approved"

	exec.Status = "failed"
	require.NoError(t, store.SavePurchaseExecution(ctx, exec))

	err := store.SaveExecutionIfStatus(ctx, &stale, []string{"running"})
	require.ErrorIs(t, err, ErrExecutionNotInExpectedStatus)
	assert.Equal(t, "failed", storedStatus(ctx, t, store, exec.ExecutionID), "stale save must not rewrite the row")

	exec.Status = "completed"
	require.NoError(t, store.SaveExecutionIfStatus(ctx, exec, []string{"failed", "running"}))
	assert.Equal(t, "completed", storedStatus(ctx, t, store, exec.ExecutionID))
}

func TestSaveExecutionIfStatus_MissingRowIsNotFoundAndNotInserted(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	exec := newNoSubmissionExec(ctx, t, store, "running")
	ghost := *exec
	ghost.ExecutionID = "00000000-0000-0000-0000-000000000abc"

	require.ErrorIs(t, store.SaveExecutionIfStatus(ctx, &ghost, []string{"running"}), ErrNotFound)
	var n int
	require.NoError(t, store.db.QueryRow(ctx, `SELECT count(*) FROM purchase_executions WHERE execution_id = $1`, ghost.ExecutionID).Scan(&n))
	assert.Zero(t, n, "a guarded save must never insert")
}

func TestSaveExecutionIfStatus_KeepsUpsertRules(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	exec := newNoSubmissionExec(ctx, t, store, "running")
	exec.Status = "failed"
	exec.NoSubmission = true
	require.NoError(t, store.SaveExecutionIfStatus(ctx, exec, []string{"running"}))
	assert.True(t, storedNoSubmission(ctx, t, store, exec.ExecutionID), "failed write keeps the proof")

	stale := *exec
	stale.NoSubmission = false
	require.NoError(t, store.SaveExecutionIfStatus(ctx, &stale, []string{"failed"}))
	assert.True(t, storedNoSubmission(ctx, t, store, exec.ExecutionID), "monotone proof rule is shared with the plain upsert")
}

// Two writers race a guarded save against a CAS-style change: exactly one
// expected-status writer can win when both target the same expected status.
func TestSaveExecutionIfStatus_ConcurrentWritersOneWins(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	exec := newNoSubmissionExec(ctx, t, store, "running")

	const n = 6
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := *exec
			c.Status = "completed"
			if i%2 == 0 {
				c.Status = "failed"
			}
			// Each writer flips running -> its status; later ones see a changed status.
			errs[i] = store.WithTx(ctx, func(tx pgx.Tx) error {
				return store.SaveExecutionIfStatusTx(ctx, tx, &c, []string{"running"})
			})
		}()
	}
	wg.Wait()
	wins := 0
	for _, e := range errs {
		if e == nil {
			wins++
			continue
		}
		require.ErrorIs(t, e, ErrExecutionNotInExpectedStatus)
	}
	assert.Equal(t, 1, wins)
}

func TestSaveExecutionIfStatusTx_RollbackLeavesRowUntouched(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	exec := newNoSubmissionExec(ctx, t, store, "running")
	exec.Status = "failed"
	sentinel := assert.AnError
	err := store.WithTx(ctx, func(tx pgx.Tx) error {
		require.NoError(t, store.SaveExecutionIfStatusTx(ctx, tx, exec, []string{"running"}))
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)
	assert.Equal(t, "running", storedStatus(ctx, t, store, exec.ExecutionID))
}

func reapedRow(ctx context.Context, t *testing.T, store *PostgresStore) *PurchaseExecution {
	t.Helper()
	exec := newNoSubmissionExec(ctx, t, store, "running")
	_, err := store.TransitionExecutionStatus(ctx, exec.ExecutionID, []string{"running"}, "failed", nil)
	require.NoError(t, err)
	exec.Status = "completed"
	exec.Error = "recovered after reap; provider purchase succeeded"
	return exec
}

func TestRecoverReapedExecution_FailedRowBecomesCompleted(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	exec := reapedRow(ctx, t, store)

	require.NoError(t, store.RecoverReapedExecution(ctx, exec))
	got, err := store.GetExecutionByID(ctx, exec.ExecutionID)
	require.NoError(t, err)
	assert.Equal(t, "completed", got.Status)
	assert.Equal(t, exec.Error, got.Error)
	assert.False(t, got.NoSubmission)

	// Reaper annotation lands after the recovery: guarded on failed, so it is
	// rejected and the completed row stays.
	late := *exec
	late.Status = "failed"
	late.Error = "reaped after 10m in running state"
	require.ErrorIs(t, store.SaveExecutionIfStatus(ctx, &late, []string{"failed"}), ErrExecutionNotInExpectedStatus)
	got, err = store.GetExecutionByID(ctx, exec.ExecutionID)
	require.NoError(t, err)
	assert.Equal(t, "completed", got.Status)
	assert.Equal(t, exec.Error, got.Error)
}

func TestRecoverReapedExecution_PartialStatusAllowed(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	exec := reapedRow(ctx, t, store)
	exec.Status = "partially_completed"
	require.NoError(t, store.RecoverReapedExecution(ctx, exec))
	assert.Equal(t, "partially_completed", storedStatus(ctx, t, store, exec.ExecutionID))
}

func TestRecoverReapedExecution_RetryLinkedIsRefused(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	exec := reapedRow(ctx, t, store)
	succ := newNoSubmissionExec(ctx, t, store, "pending")
	err := store.WithTx(ctx, func(tx pgx.Tx) error {
		linked, lerr := store.LinkRetryExecutionAtomic(ctx, tx, exec.ExecutionID, succ.ExecutionID)
		require.True(t, linked)
		return lerr
	})
	require.NoError(t, err)

	require.ErrorIs(t, store.RecoverReapedExecution(ctx, exec), ErrExecutionRetryLinked)
	assert.Equal(t, "failed", storedStatus(ctx, t, store, exec.ExecutionID))
}

func TestRecoverReapedExecution_OnlyFailedRowsAndOnlySuccessStatuses(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	exec := newNoSubmissionExec(ctx, t, store, "running")
	exec.Status = "completed"
	require.ErrorIs(t, store.RecoverReapedExecution(ctx, exec), ErrExecutionNotInExpectedStatus, "a running row is not recoverable")

	bad := reapedRow(ctx, t, store)
	bad.Status = "failed"
	require.Error(t, store.RecoverReapedExecution(ctx, bad))
	assert.Equal(t, "failed", storedStatus(ctx, t, store, bad.ExecutionID))
}

// The clear is only meaningful against a row that actually carries the proof:
// a failed+true row recovered to completed, or guard-saved to a non-failed
// status, must end with no_submission = false.
func TestGuardedSaves_ClearAProofThatWasActuallySet(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	seed := func() *PurchaseExecution {
		exec := newNoSubmissionExec(ctx, t, store, "running")
		exec.Status = "failed"
		exec.NoSubmission = true
		require.NoError(t, store.SavePurchaseExecution(ctx, exec))
		require.True(t, storedNoSubmission(ctx, t, store, exec.ExecutionID), "precondition: proof is set")
		return exec
	}

	rec := seed()
	rec.Status = "completed"
	rec.NoSubmission = false
	require.NoError(t, store.RecoverReapedExecution(ctx, rec))
	assert.False(t, storedNoSubmission(ctx, t, store, rec.ExecutionID), "recovery to completed clears the proof")

	g := seed()
	g.Status = "approved"
	g.NoSubmission = false
	require.NoError(t, store.SaveExecutionIfStatus(ctx, g, []string{"failed"}))
	assert.False(t, storedNoSubmission(ctx, t, store, g.ExecutionID), "a non-failed guarded save clears the proof")
}
