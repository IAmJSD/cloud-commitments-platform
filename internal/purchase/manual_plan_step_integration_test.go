//go:build integration

package purchase

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Real PostgreSQL and resolver; recommendation reads and email delivery are fixtures.
func TestManualPlanStep_AzureSizingPersistsWithoutAutoPurchase(t *testing.T) {
	ctx := context.Background()
	store, pool := rampStepStore(ctx, t)
	global, err := store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	recipient := "fixture@example.invalid"
	global.NotificationEmail, global.AutoPurchaseEnabled = &recipient, false
	require.NoError(t, store.SaveGlobalConfig(ctx, global))
	account := &config.CloudAccount{Name: "manual-azure", Provider: "azure", ExternalID: uuid.NewString(), Enabled: true}
	require.NoError(t, store.CreateCloudAccount(ctx, account))
	next := time.Now().Add(84 * time.Hour).Truncate(time.Microsecond)
	plan := &config.PurchasePlan{Name: "manual-azure", Enabled: true, NotificationDaysBefore: 3,
		Services:          map[string]config.ServiceConfig{"azure:compute": {Provider: "azure", Service: "compute", Enabled: true, Term: 1, Payment: "monthly", Coverage: 80}},
		RampSchedule:      config.RampSchedule{Type: "weekly", PercentPerStep: 25, StepIntervalDays: 7, TotalSteps: 4, StartDate: next},
		NextExecutionDate: &next}
	require.NoError(t, plan.Validate())
	require.NoError(t, store.CreatePurchasePlan(ctx, plan))
	require.NoError(t, store.SetPlanAccounts(ctx, plan.ID, []string{account.ID}))
	monthly := 1000.0
	rec := config.RecommendationRecord{ID: "manual-azure-rec", Provider: "azure", Service: "compute", Region: "westeurope", ResourceType: "Standard_D2s_v3",
		Count: 100, Term: 1, Payment: "monthly", MonthlyCost: &monthly, Savings: 500, CloudAccountID: &account.ID}
	sender := newCountingSender(ctx)
	m := &Manager{config: store, email: sender, resolver: planstepResolver{
		src: &cumSource{shown: func() []config.RecommendationRecord { return []config.RecommendationRecord{rec} }}, store: cumStore{rows: []config.RecommendationRecord{rec}}}}
	result, err := m.SendUpcomingPurchaseNotifications(ctx)
	require.NoError(t, err)
	assert.Zero(t, result.Notified, "manual plans do not implicitly mint roots")
	assert.Empty(t, stepRowsInDB(ctx, t, store, plan.ID))
	root := &config.PurchaseExecution{ExecutionID: uuid.NewString(), PlanID: plan.ID, Status: "pending", StepNumber: 1, ScheduledDate: next}
	require.NoError(t, store.SavePurchaseExecution(ctx, root))
	var wg sync.WaitGroup
	errors := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); _, tickErr := m.SendUpcomingPurchaseNotifications(ctx); errors <- tickErr }()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	rows := stepRowsInDB(ctx, t, store, plan.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, root.ExecutionID, rows[0].ExecutionID)
	assert.Equal(t, "notified", rows[0].Status)
	assert.True(t, rows[0].ScheduledDate.Equal(next))
	require.Len(t, rows[0].Recommendations, 1)
	assert.Equal(t, 20, rows[0].Recommendations[0].Count)
	require.NotNil(t, rows[0].Recommendations[0].MonthlyCost)
	assert.Equal(t, 200.0, *rows[0].Recommendations[0].MonthlyCost)
	assert.Equal(t, 2400.0, rows[0].Recommendations[0].TotalCommitment())
	assert.Equal(t, 100.0, rows[0].EstimatedSavings)
	assert.Zero(t, rows[0].TotalUpfrontCost)
	eligible, err := m.executableByScheduler(ctx, &rows[0])
	require.NoError(t, err)
	assert.False(t, eligible)
	var suppressed int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COALESCE(SUM(suppressed_count),0) FROM purchase_suppressions WHERE execution_id=$1`, root.ExecutionID).Scan(&suppressed))
	assert.Equal(t, 20, suppressed)
}

func TestManualPlanStep_OverdueEmptyRootFailsOnceWithoutEmail(t *testing.T) {
	ctx := context.Background()
	store, _ := rampStepStore(ctx, t)
	next := time.Now().Add(-30 * 24 * time.Hour).Truncate(time.Microsecond)
	plan := attachPlan(next, 100)
	plan.ID, plan.Name, plan.AutoPurchase = "", "overdue-manual", false
	plan.Services["azure:compute"] = config.ServiceConfig{Provider: "azure", Service: "compute", Enabled: true, Term: 1, Payment: "monthly", Coverage: 80}
	plan.RampSchedule = config.RampSchedule{Type: "weekly", PercentPerStep: 25, StepIntervalDays: 7, TotalSteps: 4, StartDate: next}
	require.NoError(t, store.CreatePurchasePlan(ctx, plan))
	root := &config.PurchaseExecution{ExecutionID: uuid.NewString(), PlanID: plan.ID, Status: "pending", StepNumber: 1, ScheduledDate: next}
	require.NoError(t, store.SavePurchaseExecution(ctx, root))
	future := time.Now().Add(20 * 24 * time.Hour).Truncate(time.Microsecond)
	plan.NextExecutionDate = &future
	require.NoError(t, store.UpdatePurchasePlan(ctx, plan))
	m := &Manager{config: store}
	for range 2 {
		result, err := m.SendUpcomingPurchaseNotifications(ctx)
		require.NoError(t, err)
		assert.Zero(t, result.Notified)
	}
	rows := stepRowsInDB(ctx, t, store, plan.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, "failed", rows[0].Status)
	assert.True(t, rows[0].NoSubmission)
	assert.Contains(t, rows[0].Error, "without recommendations")
	assert.True(t, rows[0].ScheduledDate.Equal(next))
	stored, err := store.GetPurchasePlan(ctx, plan.ID)
	require.NoError(t, err)
	assert.Zero(t, stored.RampSchedule.CurrentStep)
}
