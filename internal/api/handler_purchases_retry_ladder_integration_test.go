//go:build integration

package api

// Real PostgreSQL: Retry against ladder tranches (issue #118, executor B2 PR1).
// A settled tranche no longer nets, so a retry successor for its execution
// would be a second, approvable purchase of the same slice.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (r *ladderRetryRig) setTrancheStatus(t *testing.T, status string) {
	require.NoError(t, r.store.WithTx(context.Background(), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE ladder_tranches SET status = $2, settled_at = CASE WHEN $2 = 'completed' THEN now() END WHERE id = $1`, r.trancheID, status)
		return err
	}))
}

func (r *ladderRetryRig) executionCount(t *testing.T) int {
	var n int
	require.NoError(t, r.store.WithTx(context.Background(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM purchase_executions`).Scan(&n)
	}))
	return n
}

func (r *ladderRetryRig) retryLink(t *testing.T) *string {
	var link *string
	require.NoError(t, r.store.WithTx(context.Background(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT retry_execution_id::text FROM purchase_executions WHERE execution_id = $1`, r.failedExec.ExecutionID).Scan(&link)
	}))
	return link
}

func TestRetry_RefusesSettledLadderTranche(t *testing.T) {
	for _, status := range []string{"failed", "completed", "cancelled"} { //nolint:misspell // "cancelled" is the stored ladder status value
		t.Run(status, func(t *testing.T) {
			r := newLadderRetryRig(t)
			r.setTrancheStatus(t, status)
			before := r.executionCount(t)
			h := &Handler{config: r.store}

			err := h.persistRetrySuccessorTx(context.Background(), r.failedExec, r.successor(), nil)

			ce, ok := IsClientError(err)
			require.True(t, ok, "want a client error, got %v", err)
			assert.Equal(t, 409, ce.code)
			assert.Contains(t, err.Error(), "ladder tranche")
			assert.Equal(t, before, r.executionCount(t), "no successor row may be inserted")
			assert.Nil(t, r.retryLink(t))
		})
	}
}

func TestRetry_FiredLadderTrancheStillRetries(t *testing.T) {
	r := newLadderRetryRig(t)
	h := &Handler{config: r.store}
	require.NoError(t, h.persistRetrySuccessorTx(context.Background(), r.failedExec, r.successor(), nil))
	assert.NotNil(t, r.retryLink(t))
}

// Keys that are not tranche ids (legacy execution-id keys, "<root>:<account>"
// multi-account keys, content fingerprints, empty) are not ladder executions:
// the id::text comparison must not error on them and Retry proceeds.
func TestRetry_NonLadderKeysRetryNormally(t *testing.T) {
	keys := map[string]string{
		"multi-account": uuid.NewString() + ":" + uuid.NewString(),
		"legacy-exec":   uuid.NewString(),
		"fingerprint":   "sha256:0123abcd",
		"empty":         "",
	}
	for name, key := range keys {
		t.Run(name, func(t *testing.T) {
			r := newLadderRetryRig(t)
			r.setTrancheStatus(t, "failed") // a settled tranche exists, but the key does not name it
			r.failedExec.IdempotencyKey = key
			h := &Handler{config: r.store}

			require.NoError(t, h.persistRetrySuccessorTx(context.Background(), r.failedExec, r.successor(), nil))
			assert.NotNil(t, r.retryLink(t))
		})
	}
}

// retryLockPausedStore holds Retry's transaction open right after it locked the
// tranche row, until release is closed.
type retryLockPausedStore struct {
	*config.PostgresStore
	locked  chan struct{}
	release chan struct{}
	pid     int // backend holding the tranche row; set before locked closes
}

func (s *retryLockPausedStore) LockLadderTrancheForRetryTx(ctx context.Context, tx pgx.Tx, key string) (ladder.TrancheStatus, bool, error) {
	st, found, err := s.PostgresStore.LockLadderTrancheForRetryTx(ctx, tx, key)
	if err == nil {
		err = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&s.pid)
	}
	close(s.locked)
	<-s.release
	return st, found, err
}

// While Retry holds the tranche row (checked, successor not yet linked), the
// reconciler cannot fail the tranche: it waits, then sees the linked successor
// and leaves the tranche fired. Without the row lock the reconciler would fail
// the tranche in between and the retry would link a live successor to it.
func TestRetry_TrancheLockBlocksReconciler(t *testing.T) {
	r := newLadderRetryRig(t)
	ctx := context.Background()
	paused := &retryLockPausedStore{PostgresStore: r.store, locked: make(chan struct{}), release: make(chan struct{})}
	h := &Handler{config: paused}
	retried := make(chan error, 1)
	go func() { retried <- h.persistRetrySuccessorTx(ctx, r.failedExec, r.successor(), nil) }()
	<-paused.locked

	reconciled := make(chan error, 1)
	go func() {
		_, err := purchase.ReconcileLadderTranches(ctx, r.store, visibleNever{}, time.Now())
		reconciled <- err
	}()
	// The reconciler must queue behind Retry's tranche row lock; observing it
	// blocked proves it cannot fail the tranche before Retry links.
	if err, finished := testhelpers.AwaitBlockedBy(ctx, t, r.pool, paused.pid, reconciled); finished {
		close(paused.release) // let the Retry goroutine finish so cleanup cannot hang
		<-retried
		t.Fatalf("reconciler finished while Retry held the tranche row (err=%v)", err)
	}

	close(paused.release)
	require.NoError(t, <-retried)
	require.NoError(t, <-reconciled)
	assert.Equal(t, "fired", r.trancheStatus(t))
	assert.NotNil(t, r.retryLink(t))
}

// Reconciler first: it fails the tranche, then Retry is refused.
func TestRetry_AfterReconcileFailedIsRefused(t *testing.T) {
	r := newLadderRetryRig(t)
	ctx := context.Background()
	_, err := purchase.ReconcileLadderTranches(ctx, r.store, visibleNever{}, time.Now())
	require.NoError(t, err)
	require.Equal(t, "failed", r.trancheStatus(t))

	err = (&Handler{config: r.store}).persistRetrySuccessorTx(ctx, r.failedExec, r.successor(), nil)
	ce, ok := IsClientError(err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, 409, ce.code)
	assert.Nil(t, r.retryLink(t))
}

// Real Retry racing the real reconciler, 25 times: a failed tranche never has a
// live successor linked to its execution.
func TestRetry_ConcurrentWithReconcilerNeverLeavesFailedTrancheWithSuccessor(t *testing.T) {
	r := newLadderRetryRig(t)
	ctx := context.Background()
	h := &Handler{config: r.store}
	for i := 0; i < 25; i++ {
		if i > 0 {
			r.addFiredTranche(t)
		}
		var wg sync.WaitGroup
		var retryErr error
		wg.Add(2)
		go func() { defer wg.Done(); retryErr = h.persistRetrySuccessorTx(ctx, r.failedExec, r.successor(), nil) }()
		go func() {
			defer wg.Done()
			_, err := purchase.ReconcileLadderTranches(ctx, r.store, visibleNever{}, time.Now())
			assert.NoError(t, err)
		}()
		wg.Wait()

		status, link := r.trancheStatus(t), r.retryLink(t)
		msg := fmt.Sprintf("iteration %d: tranche=%s link=%v retryErr=%v", i, status, link, retryErr)
		if status == "failed" {
			assert.Nil(t, link, "failed tranche with a live successor: "+msg)
			assert.Error(t, retryErr, msg)
		} else {
			assert.Equal(t, "fired", status, msg)
		}
	}
}
