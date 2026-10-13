package purchase

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/planstep"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #609 (tier A): a plan step carries no recommendations, so executing it
// buys nothing. It used to finish as "completed" and advance the ramp. These
// tests use the shape of the reported plan: Azure compute, 1 year, monthly
// payment, Auto-Purchase on, a bare pending step with no recommendations.

const (
	bareStepPlanID = "plan-609"
	bareStepExecID = "exec-609"
)

func bareStepPlan() *config.PurchasePlan {
	next := time.Now().Add(24 * time.Hour)
	return &config.PurchasePlan{
		ID:                     bareStepPlanID,
		Name:                   "Azure compute 80% weekly",
		Enabled:                true,
		AutoPurchase:           true,
		NotificationDaysBefore: 7,
		NextExecutionDate:      &next,
		Services: map[string]config.ServiceConfig{
			"azure:compute": {Provider: "azure", Service: "compute", Term: 1, Payment: "monthly"},
		},
		RampSchedule: config.RampSchedule{CurrentStep: 0, TotalSteps: 4, StepIntervalDays: 7},
	}
}

func bareStepExecution() config.PurchaseExecution {
	return config.PurchaseExecution{
		ExecutionID:   bareStepExecID,
		PlanID:        bareStepPlanID,
		Status:        "pending",
		StepNumber:    1,
		ScheduledDate: time.Now().Add(-1 * time.Hour),
	}
}

// saveRecorder collects every row saved through SavePurchaseExecution. The
// fan-out saves from goroutines, so it must be mutex-guarded and read after the
// run, never asserted from inside a goroutine.
type saveRecorder struct {
	mu    sync.Mutex
	saved []config.PurchaseExecution
}

func (r *saveRecorder) save(_ context.Context, exec *config.PurchaseExecution) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saved = append(r.saved, *exec)
	return nil
}

func TestScheduledBarePlanStepFailsOnceAndDoesNotAdvanceRamp(t *testing.T) {
	// 0 is the immediate-run setting: nothing is notified ahead of time, so
	// the rec-less row reaches the sweep as "pending" with no notice sent.
	for _, days := range []int{0, 7} {
		t.Run(fmt.Sprintf("NotificationDaysBefore=%d", days), func(t *testing.T) {
			scheduledBarePlanStepFails(t, days)
		})
	}
}

func scheduledBarePlanStepFails(t *testing.T, notificationDaysBefore int) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)
	t.Cleanup(func() {
		mockStore.AssertExpectations(t)
		mockEmail.AssertExpectations(t)
	})

	plan := bareStepPlan()
	plan.NotificationDaysBefore = notificationDaysBefore
	exec := bareStepExecution()
	claimed := exec
	claimed.Status = "running"

	mockStore.On("GetStaleApprovedExecutions", ctx, mock.Anything).Return([]config.PurchaseExecution{}, nil)
	mockStore.On("GetPendingExecutions", ctx).Return([]config.PurchaseExecution{exec}, nil)
	mockStore.On("GetGlobalConfig", ctx).Return(autoPurchaseOnCfg(), nil)
	mockStore.On("GetPurchasePlan", ctx, bareStepPlanID).Return(plan, nil)
	mockStore.On("TransitionExecutionStatus", ctx, bareStepExecID,
		[]string{"approved", "pending", "notified"}, "running", (*string)(nil)).Return(&claimed, nil)
	// A two-account plan: the failure must be recorded once on the root row,
	// not once per account.
	mockStore.GetPlanAccountsFn = func(context.Context, string) ([]config.CloudAccount, error) {
		return []config.CloudAccount{
			{ID: "aaaaaaaa-0000-0000-0000-000000000001", Provider: "azure", ExternalID: "sub-1"},
			{ID: "aaaaaaaa-0000-0000-0000-000000000002", Provider: "azure", ExternalID: "sub-2"},
		}, nil
	}
	rec := &saveRecorder{}
	mockStore.SavePurchaseExecutionFn = rec.save
	// Ramp must not move: CompletePlanStep is deliberately not expected, so a
	// call fails the test as an unexpected mock call.

	manager := &Manager{config: mockStore, email: mockEmail}

	result, err := manager.ProcessScheduledPurchases(ctx)
	require.NoError(t, err)

	assert.Equal(t, 1, result.Failed, "an empty plan step is a failure, not an executed purchase")
	assert.Equal(t, 0, result.Executed)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Len(t, rec.saved, 1, "exactly one row is written: the failed root, no per-account rows")
	assert.Equal(t, bareStepExecID, rec.saved[0].ExecutionID)
	assert.Equal(t, "failed", rec.saved[0].Status)
	assert.Contains(t, rec.saved[0].Error, "no recommendations")
}

func TestNotificationTickOnBarePlanStepCreatesOneFailedRowAndSendsNoEmail(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)
	t.Cleanup(func() {
		mockStore.AssertExpectations(t)
		mockEmail.AssertExpectations(t)
	})

	plan := *bareStepPlan()
	pastDeadline := time.Now().Add(-180 * time.Hour) // inside the one-day retry window after the grace
	plan.NextExecutionDate = &pastDeadline
	plan.Services = map[string]config.ServiceConfig{"azure:compute": {Enabled: true, Provider: "azure", Service: "compute", Term: 1, Payment: "monthly"}}
	notifyTo := "notify@example.com"

	mockStore.On("ListPurchasePlans", ctx, config.PurchasePlanFilter{}).Return([]config.PurchasePlan{plan}, nil)
	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{NotificationEmail: &notifyTo, AutoPurchaseEnabled: true}, nil)

	// Tick 1 finds no row for the date; tick 2 finds whatever tick 1 saved.
	var stored config.PurchaseExecution
	var saves []config.PurchaseExecution
	mockStore.On("LockPurchasePlanTx", mock.Anything, mock.Anything, plan.ID).Return(&plan, nil)
	mockStore.On("ListExecutionsForPlanStepTx", mock.Anything, mock.Anything, plan.ID, plan.RampSchedule.CurrentStep+1).
		Return(nil, nil).Once()
	mockStore.SavePurchaseExecutionFn = func(_ context.Context, exec *config.PurchaseExecution) error {
		stored = *exec
		saves = append(saves, *exec)
		// From now on the step has a row, as the real store would report.
		mockStore.On("ListExecutionsForPlanStepTx", mock.Anything, mock.Anything, plan.ID, plan.RampSchedule.CurrentStep+1).
			Return([]config.PurchaseExecution{stored}, nil)
		return nil
	}
	// A $0 approval email must never go out; Maybe() keeps a violation a clean
	// assertion failure instead of an unexpected-call panic.
	mockEmail.On("SendScheduledPurchaseNotification", ctx, mock.Anything).Return(nil).Maybe()
	mockStore.On("StampPlanNotificationSent", ctx, plan.ID, mock.Anything).Return(nil).Maybe()

	manager := &Manager{config: mockStore, email: mockEmail, notifyDays: 7, resolver: &fakeResolver{err: planstep.ErrNoMatch}}

	for tick := 1; tick <= 2; tick++ {
		result, err := manager.SendUpcomingPurchaseNotifications(ctx)
		require.NoError(t, err, "tick %d", tick)
		assert.Equal(t, 0, result.Notified, "tick %d", tick)
	}

	mockEmail.AssertNotCalled(t, "SendScheduledPurchaseNotification", ctx, mock.Anything)
	require.Len(t, saves, 1, "two ticks must leave exactly one row")
	assert.Equal(t, "failed", saves[0].Status)
	assert.Equal(t, 1, saves[0].StepNumber)
	assert.Contains(t, saves[0].Error, "no current recommendation matched")
}
