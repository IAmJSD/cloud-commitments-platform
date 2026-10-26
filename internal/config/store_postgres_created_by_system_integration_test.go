//go:build integration
// +build integration

package config

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #233: created_by_system round-trips through the full-row readers and is insert-only.
func TestIntegration_PurchaseExecution_CreatedBySystem(t *testing.T) {
	ctx := context.Background()
	store := setupRampStepStore(ctx, t)

	next := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	plan := &PurchasePlan{
		Name: "sys-flag", Enabled: true, AutoPurchase: true,
		Services:          map[string]ServiceConfig{"ec2": {Provider: "aws", Service: "ec2", Enabled: true}},
		RampSchedule:      RampSchedule{Type: "weekly", PercentPerStep: 25, StepIntervalDays: 7, TotalSteps: 4, StartDate: time.Now().AddDate(0, 0, -7)},
		NextExecutionDate: &next,
	}
	require.NoError(t, plan.Validate())
	require.NoError(t, store.CreatePurchasePlan(ctx, plan))

	exec := &PurchaseExecution{
		PlanID: plan.ID, ExecutionID: uuid.New().String(), IdempotencyKey: uuid.New().String(),
		Status: "pending", StepNumber: 1, ScheduledDate: time.Now().Add(-time.Minute), CreatedBySystem: true,
	}
	require.NoError(t, store.SavePurchaseExecution(ctx, exec))

	assertFlag := func(name string, got []PurchaseExecution, err error) {
		t.Helper()
		require.NoError(t, err, name)
		for i := range got {
			if got[i].ExecutionID == exec.ExecutionID {
				assert.True(t, got[i].CreatedBySystem, name)
				return
			}
		}
		t.Errorf("%s: execution not returned", name)
	}

	byID, err := store.GetExecutionByID(ctx, exec.ExecutionID)
	require.NoError(t, err)
	assert.True(t, byID.CreatedBySystem, "GetExecutionByID")
	got, err := store.GetPendingExecutions(ctx)
	assertFlag("GetPendingExecutions", got, err)
	got, err = store.GetExecutionsByStatuses(ctx, []string{"pending"}, 100)
	assertFlag("GetExecutionsByStatuses", got, err)
	require.NoError(t, store.WithTx(ctx, func(tx pgx.Tx) error {
		rows, txErr := store.ListExecutionsForPlanStepTx(ctx, tx, plan.ID, 1)
		assertFlag("ListExecutionsForPlanStepTx", rows, txErr)
		_, txErr = store.ListPlanCommittedTx(ctx, tx, plan.ID)
		require.NoError(t, txErr, "ListPlanCommittedTx")
		return nil
	}))

	// Insert-only: a re-save with the flag cleared must not clear it.
	exec.CreatedBySystem = false
	require.NoError(t, store.SavePurchaseExecution(ctx, exec))
	byID, err = store.GetExecutionByID(ctx, exec.ExecutionID)
	require.NoError(t, err)
	assert.True(t, byID.CreatedBySystem, "upsert must not clear the flag")

	// ...and a re-save with the flag set must not set it on an unflagged row.
	plain := &PurchaseExecution{ExecutionID: uuid.New().String(), IdempotencyKey: uuid.New().String(),
		Status: "pending", StepNumber: 1, ScheduledDate: time.Now()}
	require.NoError(t, store.SavePurchaseExecution(ctx, plain))
	plain.CreatedBySystem = true
	require.NoError(t, store.SavePurchaseExecution(ctx, plain))
	byID, err = store.GetExecutionByID(ctx, plain.ExecutionID)
	require.NoError(t, err)
	assert.False(t, byID.CreatedBySystem, "upsert must not set the flag")
}
