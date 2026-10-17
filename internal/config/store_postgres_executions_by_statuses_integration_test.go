//go:build integration
// +build integration

package config

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntegration_GetExecutionsByStatuses_RowClasses runs the real query over
// each row class the History view cares about. A clean completed execution
// (status completed, empty error) must be excluded; a completed execution with
// an error (the audit-gap case) and every other requested status must be
// returned, newest first and bounded by the limit.
func TestIntegration_GetExecutionsByStatuses_RowClasses(t *testing.T) {
	ctx := context.Background()
	store := setupRampStepStore(ctx, t)

	base := time.Now().Add(-time.Hour)
	save := func(offset int, status, execErr string) string {
		exec := &PurchaseExecution{
			ExecutionID:    uuid.New().String(),
			IdempotencyKey: uuid.New().String(),
			Status:         status,
			Error:          execErr,
			ScheduledDate:  base.Add(time.Duration(offset) * time.Minute),
		}
		require.NoError(t, store.SavePurchaseExecution(ctx, exec))
		return exec.ExecutionID
	}

	save(1, "completed", "")
	auditGap := save(2, "completed", "history write failed")
	failed := save(3, "failed", "boom")
	pending := save(4, "pending", "")

	got, err := store.GetExecutionsByStatuses(ctx, []string{"pending", "completed", "failed"}, 100)
	require.NoError(t, err)
	ids := make([]string, len(got))
	for i := range got {
		ids[i] = got[i].ExecutionID
	}
	assert.Equal(t, []string{pending, failed, auditGap}, ids, "clean completed excluded; newest first")

	limited, err := store.GetExecutionsByStatuses(ctx, []string{"pending", "completed", "failed"}, 2)
	require.NoError(t, err)
	require.Len(t, limited, 2)
	assert.Equal(t, pending, limited[0].ExecutionID)
	assert.Equal(t, failed, limited[1].ExecutionID)
}

// TestIntegration_GetExecutionsByStatuses_DueRowSurvivesFutureRampSteps is the
// issue #680 regression. Plan creation writes up to 52 pending ramp steps per
// plan, all dated in the future, so 20 plans put 1040 future pending rows in
// the actionable class. Under a plain scheduled_date DESC those fill the
// MaxListLimit cap and the pending approval due today never comes back. Every
// row that is not a future pending step must come first, newest first as
// before; future pending steps follow, soonest first.
func TestIntegration_GetExecutionsByStatuses_DueRowSurvivesFutureRampSteps(t *testing.T) {
	ctx := context.Background()
	store := setupRampStepStore(ctx, t)

	now := time.Now()
	save := func(planID, status string, scheduled time.Time) string {
		exec := &PurchaseExecution{
			PlanID:         planID,
			ExecutionID:    uuid.New().String(),
			IdempotencyKey: uuid.New().String(),
			Status:         status,
			ScheduledDate:  scheduled,
		}
		require.NoError(t, store.SavePurchaseExecution(ctx, exec))
		return exec.ExecutionID
	}

	const plans, stepsPerPlan = 20, 52
	var planIDs []string
	for p := 0; p < plans; p++ {
		plan := &PurchasePlan{
			Name:     fmt.Sprintf("Ramp %d %s", p, uuid.New().String()[:8]),
			Services: map[string]ServiceConfig{},
			RampSchedule: RampSchedule{
				Type: "weekly", PercentPerStep: 2, StepIntervalDays: 7,
				TotalSteps: stepsPerPlan, StartDate: now,
			},
		}
		require.NoError(t, store.CreatePurchasePlan(ctx, plan))
		planIDs = append(planIDs, plan.ID)
		for s := 1; s <= stepsPerPlan; s++ {
			save(plan.ID, "pending", now.AddDate(0, 0, 7*s).Add(time.Duration(p)*time.Minute))
		}
	}
	dueToday := save(planIDs[0], "pending", now.Add(-time.Hour))
	overdue := save(planIDs[1], "pending", now.Add(-48*time.Hour))
	// A notified row stays actionable with a future scheduled_date: its
	// approval link is already out.
	notifiedFuture := save(planIDs[2], "notified", now.Add(72*time.Hour))
	scheduledSoon := save(planIDs[3], "scheduled", now.Add(10*time.Minute))

	actionable := []string{"pending", "notified", "scheduled", "approved", "running", "paused"}
	got, err := store.GetExecutionsByStatuses(ctx, actionable, MaxListLimit)
	require.NoError(t, err)
	require.Len(t, got, MaxListLimit, "1044 rows still hit the cap, so the History handler sets truncated")

	head := make([]string, 4)
	for i := range head {
		head[i] = got[i].ExecutionID
	}
	assert.Equal(t, []string{notifiedFuture, scheduledSoon, dueToday, overdue}, head,
		"rows actionable now come first, newest first as before")

	for i := 4; i < len(got); i++ {
		require.Equal(t, "pending", got[i].Status)
		require.True(t, got[i].ScheduledDate.After(now), "only future pending steps follow")
		if i > 4 {
			require.False(t, got[i].ScheduledDate.Before(got[i-1].ScheduledDate),
				"future pending steps are soonest first, so the next ramp steps survive the cap")
		}
	}

	small, err := store.GetExecutionsByStatuses(ctx, actionable, 3)
	require.NoError(t, err)
	require.Len(t, small, 3)
	assert.Equal(t, []string{notifiedFuture, scheduledSoon, dueToday},
		[]string{small[0].ExecutionID, small[1].ExecutionID, small[2].ExecutionID})
}

// TestIntegration_GetExecutionsByStatuses_TerminalOrderUnchanged pins that the
// #680 ordering only moves future pending rows: a canceled ramp step keeps its
// future scheduled_date, and the terminal class must still list newest first.
func TestIntegration_GetExecutionsByStatuses_TerminalOrderUnchanged(t *testing.T) {
	ctx := context.Background()
	store := setupRampStepStore(ctx, t)

	now := time.Now()
	save := func(status string, scheduled time.Time) string {
		exec := &PurchaseExecution{
			ExecutionID:    uuid.New().String(),
			IdempotencyKey: uuid.New().String(),
			Status:         status,
			Error:          "x",
			ScheduledDate:  scheduled,
		}
		require.NoError(t, store.SavePurchaseExecution(ctx, exec))
		return exec.ExecutionID
	}
	canceledFuture := save(StatusCanceled, now.AddDate(0, 3, 0))
	failedPast := save("failed", now.Add(-time.Hour))
	expiredOlder := save("expired", now.AddDate(0, 0, -10))

	got, err := store.GetExecutionsByStatuses(ctx, []string{"failed", "expired", StatusCanceled}, 100)
	require.NoError(t, err)
	ids := make([]string, len(got))
	for i := range got {
		ids[i] = got[i].ExecutionID
	}
	assert.Equal(t, []string{canceledFuture, failedPast, expiredOlder}, ids)
}
