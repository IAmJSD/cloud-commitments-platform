//go:build integration

package config

// Real PostgreSQL tests for the persisted no_submission proof (issue #118,
// PR-C1). No mocks: the upsert and the readers run against a migrated schema.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newNoSubmissionExec(ctx context.Context, t *testing.T, store *PostgresStore, status string) *PurchaseExecution {
	t.Helper()
	var planID string
	require.NoError(t, store.db.QueryRow(ctx, `INSERT INTO purchase_plans (name) VALUES ('c1-proof') RETURNING id`).Scan(&planID))
	exec := &PurchaseExecution{PlanID: planID, Status: status, StepNumber: 1, ScheduledDate: time.Now()}
	require.NoError(t, store.SavePurchaseExecution(ctx, exec))
	return exec
}

func storedNoSubmission(ctx context.Context, t *testing.T, store *PostgresStore, id string) bool {
	t.Helper()
	var v bool
	require.NoError(t, store.db.QueryRow(ctx, `SELECT no_submission FROM purchase_executions WHERE execution_id = $1`, id).Scan(&v))
	return v
}

// Among failed writes the proof is monotone: a stale failed copy (false)
// upserted over a failed row that carries the proof (true) must not clear it,
// while false -> true works. A plain overwrite would fail the last assertion.
func TestNoSubmission_UpsertIsMonotone(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	exec := newNoSubmissionExec(ctx, t, store, "running")
	assert.False(t, storedNoSubmission(ctx, t, store, exec.ExecutionID), "default is no proof")

	stale := *exec
	exec.Status = "failed"
	exec.NoSubmission = true
	require.NoError(t, store.SavePurchaseExecution(ctx, exec))
	assert.True(t, storedNoSubmission(ctx, t, store, exec.ExecutionID), "false -> true must persist")

	stale.Status = "failed"
	stale.NoSubmission = false
	require.NoError(t, store.SavePurchaseExecution(ctx, &stale))
	assert.True(t, storedNoSubmission(ctx, t, store, exec.ExecutionID), "a stale false upsert must not clear the proof")
}

// Every reader of purchase_executions returns the flag (the shared column list
// is edited in twelve SELECT/RETURNING sites).
func TestNoSubmission_ReadersReturnTheFlag(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	exec := newNoSubmissionExec(ctx, t, store, "running")
	exec.Status = "failed" // the proof is only kept on a failed write
	exec.NoSubmission = true
	require.NoError(t, store.SavePurchaseExecution(ctx, exec))

	got, err := store.GetExecutionByID(ctx, exec.ExecutionID)
	require.NoError(t, err)
	assert.True(t, got.NoSubmission, "GetExecutionByID")

	byPlan, err := store.GetExecutionByPlanAndDate(ctx, exec.PlanID, exec.ScheduledDate)
	require.NoError(t, err)
	assert.True(t, byPlan.NoSubmission, "GetExecutionByPlanAndDate")

	time.Sleep(1100 * time.Millisecond) // updated_at is trigger-managed; age the row past the 1s floor
	stuck, err := store.ListStuckExecutions(ctx, []string{"failed"}, time.Second)
	require.NoError(t, err)
	require.Len(t, stuck, 1)
	assert.True(t, stuck[0].NoSubmission, "ListStuckExecutions")

	failed, err := store.TransitionExecutionStatus(ctx, exec.ExecutionID, []string{"failed"}, "approved", nil)
	require.NoError(t, err)
	assert.True(t, failed.NoSubmission, "TransitionExecutionStatus RETURNING")
}

// Defense in depth: only a failed write may keep the proof. A completed write
// (or any other status) over a failed+true row clears it.
func TestNoSubmission_NonFailedWriteClearsProof(t *testing.T) {
	ctx := context.Background()
	for _, status := range []string{"completed", "approved"} {
		t.Run(status, func(t *testing.T) {
			store := setupLadderStore(ctx, t)
			exec := newNoSubmissionExec(ctx, t, store, "running")
			stale := *exec
			exec.Status = "failed"
			exec.NoSubmission = true
			require.NoError(t, store.SavePurchaseExecution(ctx, exec))
			require.True(t, storedNoSubmission(ctx, t, store, exec.ExecutionID))

			stale.Status = status
			stale.NoSubmission = false
			require.NoError(t, store.SavePurchaseExecution(ctx, &stale))
			assert.False(t, storedNoSubmission(ctx, t, store, exec.ExecutionID), "a %s write must clear the proof", status)
		})
	}
}

// Replays the approve-path sequence at the store level (approvals.go:392-421):
// the approve path keeps a stale copy of the approved row; a concurrent
// claimAndExecute fails the row before submission (proof true); the approve
// path's audit upsert puts the row back to approved; the approve path then
// calls the provider and the row ends failed with outcome unknown (flag
// false). The proof must not survive on a row that reached the provider.
func TestNoSubmission_StaleApproveUpsertDoesNotKeepProofOnSubmittedRow(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	exec := newNoSubmissionExec(ctx, t, store, "approved")
	staleApproved := *exec

	exec.Status = "failed" // claimAndExecute: refused before submission
	exec.NoSubmission = true
	require.NoError(t, store.SavePurchaseExecution(ctx, exec))

	require.NoError(t, store.SavePurchaseExecution(ctx, &staleApproved)) // approvals.go:412
	var status string
	require.NoError(t, store.db.QueryRow(ctx, `SELECT status FROM purchase_executions WHERE execution_id = $1`, exec.ExecutionID).Scan(&status))
	require.Equal(t, "approved", status)
	assert.False(t, storedNoSubmission(ctx, t, store, exec.ExecutionID), "proof must be cleared when the row goes back to approved")

	final := staleApproved
	final.Status = "failed" // provider called, outcome unknown: no proof
	final.NoSubmission = false
	require.NoError(t, store.SavePurchaseExecution(ctx, &final))
	assert.False(t, storedNoSubmission(ctx, t, store, exec.ExecutionID), "a failed row that reached the provider must carry no proof")
}
