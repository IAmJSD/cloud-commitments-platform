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

// The proof is monotone: a stale in-memory copy (false) upserted over a row
// that already carries the proof (true) must not clear it, while false -> true
// works. A plain overwrite would fail the second assertion.
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
	exec := newNoSubmissionExec(ctx, t, store, "approved")
	exec.NoSubmission = true
	require.NoError(t, store.SavePurchaseExecution(ctx, exec))

	got, err := store.GetExecutionByID(ctx, exec.ExecutionID)
	require.NoError(t, err)
	assert.True(t, got.NoSubmission, "GetExecutionByID")

	byPlan, err := store.GetExecutionByPlanAndDate(ctx, exec.PlanID, exec.ScheduledDate)
	require.NoError(t, err)
	assert.True(t, byPlan.NoSubmission, "GetExecutionByPlanAndDate")

	time.Sleep(1100 * time.Millisecond) // updated_at is trigger-managed; age the row past the 1s floor
	stuck, err := store.ListStuckExecutions(ctx, []string{"approved"}, time.Second)
	require.NoError(t, err)
	require.Len(t, stuck, 1)
	assert.True(t, stuck[0].NoSubmission, "ListStuckExecutions")

	failed, err := store.TransitionExecutionStatus(ctx, exec.ExecutionID, []string{"approved"}, "failed", nil)
	require.NoError(t, err)
	assert.True(t, failed.NoSubmission, "TransitionExecutionStatus RETURNING")
}
