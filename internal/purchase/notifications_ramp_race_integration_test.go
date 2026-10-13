//go:build integration

package purchase

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestNotificationStampPreservesCompletedRampAndNextExecution(t *testing.T) {
	ctx := context.Background()
	store, _ := rampStepStore(ctx, t)
	global, err := store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	recipient := "fixture@example.invalid"
	global.NotificationEmail = &recipient
	require.NoError(t, store.SaveGlobalConfig(ctx, global))
	next := time.Now().Add(24 * time.Hour).Truncate(time.Microsecond)
	plan := &config.PurchasePlan{
		Name: "notification race", Enabled: true, AutoPurchase: true, NotificationDaysBefore: 30,
		Services: map[string]config.ServiceConfig{"ec2": {Provider: "aws", Service: "ec2"}},
		RampSchedule: config.RampSchedule{Type: "weekly", PercentPerStep: 25, StepIntervalDays: 7,
			CurrentStep: 1, TotalSteps: 4, StartDate: next.AddDate(0, 0, -7)},
		NextExecutionDate: &next,
	}
	require.NoError(t, plan.Validate())
	require.NoError(t, store.CreatePurchasePlan(ctx, plan))
	// A plan without max_step_commitment refuses any step with recommendations,
	// which gives the step its one failed row.
	manager := &Manager{config: store, resolver: &fakeResolver{recs: []config.RecommendationRecord{attachRec("rec-a", 1)}}}

	// A step-2 execution completes (and advances the ramp) while a reminder for
	// the same plan is in flight. Plan steps carry no recommendations (#609), so
	// the reminder path no longer creates such a row itself; the row is written
	// directly, and the stamp the reminder issues afterwards is called as the
	// notification path does once its email has gone out.
	step2 := &config.PurchaseExecution{
		PlanID: plan.ID, ExecutionID: uuid.New().String(), Status: "completed", StepNumber: 2,
		ScheduledDate:   next,
		Recommendations: []config.RecommendationRecord{{Provider: "aws", Service: "ec2", Count: 1}},
	}
	require.NoError(t, store.SavePurchaseExecution(ctx, step2))
	require.NoError(t, store.CompletePlanStep(ctx, plan.ID, 2))
	completed, err := store.GetPurchasePlan(ctx, plan.ID)
	require.NoError(t, err)
	require.Equal(t, 2, completed.RampSchedule.CurrentStep)
	require.NotNil(t, completed.LastExecutionDate)
	require.True(t, completed.NextExecutionDate.After(next))
	require.NoError(t, store.StampPlanNotificationSent(ctx, plan.ID, time.Now()))
	after, err := store.GetPurchasePlan(ctx, plan.ID)
	require.NoError(t, err)
	require.NotNil(t, after.LastNotificationSent)
	require.Equal(t, completed.RampSchedule.CurrentStep, after.RampSchedule.CurrentStep)
	require.Equal(t, completed.NextExecutionDate, after.NextExecutionDate)
	require.Equal(t, completed.LastExecutionDate, after.LastExecutionDate)

	// The step is refused by the cap (no max_step_commitment): the reminder path
	// records the row as failed. The step stamp and date it carries are what matters.
	_, _, _, err = manager.getOrCreateExecution(ctx, after)
	require.ErrorIs(t, err, errExecutionNotNotifiable)
	execution, err := store.GetExecutionByPlanAndDate(ctx, plan.ID, *completed.NextExecutionDate)
	require.NoError(t, err)
	require.Equal(t, "failed", execution.Status)
	require.Equal(t, 3, execution.StepNumber)
	require.Equal(t, *completed.NextExecutionDate, execution.ScheduledDate)
	execution.Status = "completed"
	require.NoError(t, store.SavePurchaseExecution(ctx, execution))
	require.NoError(t, store.CompletePlanStep(ctx, plan.ID, 3))
	continued, err := store.GetPurchasePlan(ctx, plan.ID)
	require.NoError(t, err)
	require.Equal(t, 3, continued.RampSchedule.CurrentStep)
	require.True(t, continued.NextExecutionDate.After(*after.NextExecutionDate))
	require.Equal(t, after.LastNotificationSent, continued.LastNotificationSent)
}
