//go:build integration

package api

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// W17: the retry successor insert holds a FK key-share on the plan row, so it
// queues behind the command's plan lock, and its link CAS rechecks the
// original after the wait. Retries are never refused on a managed plan: a
// failed row is fixed in the editor.
func retryRace(ctx context.Context, t *testing.T) (*createConcurrencyFixture, *config.PurchaseExecution, func() error) {
	f := newCreateConcurrencyFixture(ctx, t)
	failed := &config.PurchaseExecution{ExecutionID: uuid.NewString(), PlanID: f.planID, Status: "failed", StepNumber: 3, ScheduledDate: time.Now()}
	require.NoError(t, f.store.SavePurchaseExecution(ctx, failed))
	successor := &config.PurchaseExecution{ExecutionID: uuid.NewString(), PlanID: f.planID, Status: "pending", StepNumber: 3, ScheduledDate: time.Now(), RetryAttemptN: 1}
	return f, successor, func() error { return f.handler.persistRetrySuccessorTx(ctx, failed, successor, nil) }
}

func TestRetryWaitsForCommandThenSucceedsAndCommandSeesIt(t *testing.T) {
	ctx := context.Background()
	f, successor, retry := retryRace(ctx, t)
	command := f.holdPlanLock(ctx, t)
	done := make(chan error, 1)
	go func() { done <- retry() }()
	testhelpers.WaitForLockWait(ctx, t, f.pool, "%INSERT INTO purchase_executions%")
	command.SetManaged(ctx, t, f.planID)
	require.NoError(t, command.Commit(ctx))
	require.NoError(t, testhelpers.Await(t, done), "a retry of a failed row is allowed on a managed plan")

	require.Contains(t, f.holdPlanLock(ctx, t).Executions, successor.ExecutionID, "the next command capture sees the successor")
}

func TestRetryLosesToCommandThatLinkedOriginalFirst(t *testing.T) {
	ctx := context.Background()
	f, successor, retry := retryRace(ctx, t)
	command := f.holdPlanLock(ctx, t)
	done := make(chan error, 1)
	go func() { done <- retry() }()
	testhelpers.WaitForLockWait(ctx, t, f.pool, "%INSERT INTO purchase_executions%")
	other := uuid.NewString()
	_, err := command.Exec(ctx, `INSERT INTO purchase_executions (execution_id, plan_id, status, step_number, scheduled_date) VALUES ($1, $2, 'pending', 3, now())`, other, f.planID)
	require.NoError(t, err)
	_, err = command.Exec(ctx, `UPDATE purchase_executions SET retry_execution_id = $1 WHERE plan_id = $2 AND status = 'failed'`, other, f.planID)
	require.NoError(t, err)
	require.NoError(t, command.Commit(ctx))

	ce, ok := IsClientError(testhelpers.Await(t, done))
	require.True(t, ok)
	require.Equal(t, 409, ce.code)
	require.Contains(t, ce.message, "already retried")
	require.Equal(t, 2, f.count(ctx, t, execCount), "the lost retry leaves no successor row")
	_, err = f.store.GetExecutionByID(ctx, successor.ExecutionID)
	require.ErrorIs(t, err, config.ErrNotFound)
}
