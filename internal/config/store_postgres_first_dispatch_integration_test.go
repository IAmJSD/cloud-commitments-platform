//go:build integration
// +build integration

package config

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const stampTestHorizon = 24 * time.Hour

func stampTx(ctx context.Context, t *testing.T, store *PostgresStore, id string) (StampResult, error) {
	t.Helper()
	var res StampResult
	err := store.WithTx(ctx, func(tx pgx.Tx) error {
		var e error
		res, e = store.StampFirstDispatchTx(ctx, tx, id, stampTestHorizon)
		return e
	})
	return res, err
}

func storedStamp(ctx context.Context, t *testing.T, store *PostgresStore, id string) *time.Time {
	t.Helper()
	var v *time.Time
	require.NoError(t, store.db.QueryRow(ctx, `SELECT first_dispatch_at FROM purchase_executions WHERE execution_id = $1`, id).Scan(&v))
	return v
}

// seedPredecessor saves a failed, stamp-less row pointing at successorID.
func seedPredecessor(ctx context.Context, t *testing.T, store *PostgresStore, successorID string, attemptN int, noSubmission bool) {
	t.Helper()
	pred := newNoSubmissionExec(ctx, t, store, "failed")
	_, err := store.db.Exec(ctx, `UPDATE purchase_executions SET retry_execution_id = $2, retry_attempt_n = $3, no_submission = $4 WHERE execution_id = $1`,
		pred.ExecutionID, successorID, attemptN, noSubmission)
	require.NoError(t, err)
}

func seedRunningSuccessor(ctx context.Context, t *testing.T, store *PostgresStore, attemptN int, stamp *time.Time) *PurchaseExecution {
	t.Helper()
	succ := newNoSubmissionExec(ctx, t, store, "running")
	_, err := store.db.Exec(ctx, `UPDATE purchase_executions SET retry_attempt_n = $2, first_dispatch_at = $3 WHERE execution_id = $1`, succ.ExecutionID, attemptN, stamp)
	require.NoError(t, err)
	return succ
}

func TestStampFirstDispatch_MintsOnceAndIsStable(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	exec := newNoSubmissionExec(ctx, t, store, "running")

	first, err := stampTx(ctx, t, store, exec.ExecutionID)
	require.NoError(t, err)
	require.NotNil(t, first.At)
	assert.True(t, first.WithinHorizon)
	assert.WithinDuration(t, time.Now(), *first.At, time.Minute)

	time.Sleep(20 * time.Millisecond)
	second, err := stampTx(ctx, t, store, exec.ExecutionID)
	require.NoError(t, err)
	require.NotNil(t, second.At)
	assert.True(t, first.At.Equal(*second.At), "a second stamp must return the first one")
	assert.True(t, first.At.Equal(*storedStamp(ctx, t, store, exec.ExecutionID)))
}

func TestStampFirstDispatch_RetrySuccessorKeepsCopiedStamp(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	copied := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Microsecond)
	succ := seedRunningSuccessor(ctx, t, store, 1, &copied)

	res, err := stampTx(ctx, t, store, succ.ExecutionID)
	require.NoError(t, err)
	require.NotNil(t, res.At)
	assert.True(t, copied.Equal(*res.At), "a copied stamp must not be re-minted")
	assert.True(t, res.WithinHorizon)
}

func TestStampFirstDispatch_RetryWithoutStampIsNotMinted(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	succ := seedRunningSuccessor(ctx, t, store, 1, nil)
	seedPredecessor(ctx, t, store, succ.ExecutionID, 0, false)

	res, err := stampTx(ctx, t, store, succ.ExecutionID)
	require.NoError(t, err)
	assert.Nil(t, res.At)
	assert.False(t, res.WithinHorizon)
	assert.Nil(t, storedStamp(ctx, t, store, succ.ExecutionID))
}

func TestStampFirstDispatch_RetryOfNoSubmissionAttempt0Mints(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	succ := seedRunningSuccessor(ctx, t, store, 1, nil)
	seedPredecessor(ctx, t, store, succ.ExecutionID, 0, true)

	res, err := stampTx(ctx, t, store, succ.ExecutionID)
	require.NoError(t, err)
	require.NotNil(t, res.At)
	assert.True(t, res.WithinHorizon)
}

// A chain of length >= 2 may have submitted at attempt 0 even though the
// immediate predecessor carries no_submission, so it must not mint.
func TestStampFirstDispatch_RetryOfNoSubmissionAttempt1DoesNotMint(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	succ := seedRunningSuccessor(ctx, t, store, 2, nil)
	seedPredecessor(ctx, t, store, succ.ExecutionID, 1, true)

	res, err := stampTx(ctx, t, store, succ.ExecutionID)
	require.NoError(t, err)
	assert.Nil(t, res.At)
	assert.Nil(t, storedStamp(ctx, t, store, succ.ExecutionID))
}

func TestStampFirstDispatch_NotRunning(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	for _, status := range []string{"approved", "failed", "completed"} {
		exec := newNoSubmissionExec(ctx, t, store, status)
		_, err := stampTx(ctx, t, store, exec.ExecutionID)
		require.ErrorIs(t, err, ErrExecutionNotInExpectedStatus, status)
		assert.Nil(t, storedStamp(ctx, t, store, exec.ExecutionID), status)
	}
}

// The horizon is judged by the database clock, not the app's.
func TestStampFirstDispatch_WithinHorizonUsesDBClock(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	old := time.Now().Add(-stampTestHorizon - time.Hour).UTC()
	recent := time.Now().Add(-stampTestHorizon + time.Hour).UTC()

	pastSucc := seedRunningSuccessor(ctx, t, store, 1, &old)
	res, err := stampTx(ctx, t, store, pastSucc.ExecutionID)
	require.NoError(t, err)
	require.NotNil(t, res.At)
	assert.False(t, res.WithinHorizon)

	inSucc := seedRunningSuccessor(ctx, t, store, 1, &recent)
	res, err = stampTx(ctx, t, store, inSucc.ExecutionID)
	require.NoError(t, err)
	assert.True(t, res.WithinHorizon)
}

// now() is the tx start, so a stamp seeded at now() - H inside the same tx is
// exactly H old: that is outside the horizon.
func TestStampFirstDispatch_ExactHorizonIsOutside(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	succ := seedRunningSuccessor(ctx, t, store, 1, nil)

	var res StampResult
	require.NoError(t, store.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE purchase_executions SET first_dispatch_at = now() - make_interval(secs => $2) WHERE execution_id = $1`,
			succ.ExecutionID, stampTestHorizon.Seconds()); err != nil {
			return err
		}
		var err error
		res, err = store.StampFirstDispatchTx(ctx, tx, succ.ExecutionID, stampTestHorizon)
		return err
	}))
	require.NotNil(t, res.At)
	assert.False(t, res.WithinHorizon)
}
