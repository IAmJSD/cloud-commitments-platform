package purchase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/email"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/planstep"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestManager_SendUpcomingPurchaseNotifications_NoPlans(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)

	mockStore.On("ListPurchasePlans", ctx, config.PurchasePlanFilter{}).Return([]config.PurchasePlan{}, nil)

	manager := &Manager{
		config:       mockStore,
		email:        mockEmail,
		notifyDays:   7,
		dashboardURL: "https://dashboard.example.com",
	}

	result, err := manager.SendUpcomingPurchaseNotifications(ctx)
	require.NoError(t, err)

	assert.Equal(t, 0, result.Notified)

	mockStore.AssertExpectations(t)
}

func TestManager_SendUpcomingPurchaseNotifications_DisabledPlan(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)

	plans := []config.PurchasePlan{
		{
			ID:           "plan-123",
			Name:         "Test Plan",
			Enabled:      false,
			AutoPurchase: true,
		},
	}

	mockStore.On("ListPurchasePlans", ctx, config.PurchasePlanFilter{}).Return(plans, nil)

	manager := &Manager{
		config:       mockStore,
		email:        mockEmail,
		notifyDays:   7,
		dashboardURL: "https://dashboard.example.com",
	}

	result, err := manager.SendUpcomingPurchaseNotifications(ctx)
	require.NoError(t, err)

	assert.Equal(t, 0, result.Notified)

	mockStore.AssertExpectations(t)
}

func TestManager_SendUpcomingPurchaseNotifications_NotAutoPurchase(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)

	plans := []config.PurchasePlan{
		{
			ID:           "plan-123",
			Name:         "Test Plan",
			Enabled:      true,
			AutoPurchase: false,
		},
	}

	mockStore.On("ListPurchasePlans", ctx, config.PurchasePlanFilter{}).Return(plans, nil)

	manager := &Manager{
		config:       mockStore,
		email:        mockEmail,
		notifyDays:   7,
		dashboardURL: "https://dashboard.example.com",
	}

	result, err := manager.SendUpcomingPurchaseNotifications(ctx)
	require.NoError(t, err)

	assert.Equal(t, 0, result.Notified)

	mockStore.AssertExpectations(t)
}

func TestManager_SendUpcomingPurchaseNotifications_Error(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)

	mockStore.On("ListPurchasePlans", ctx, config.PurchasePlanFilter{}).Return(nil, errors.New("database error"))

	manager := &Manager{
		config:       mockStore,
		email:        mockEmail,
		notifyDays:   7,
		dashboardURL: "https://dashboard.example.com",
	}

	result, err := manager.SendUpcomingPurchaseNotifications(ctx)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "failed to list purchase plans")

	mockStore.AssertExpectations(t)
}

func TestManager_BuildNotificationData(t *testing.T) {
	manager := &Manager{
		dashboardURL: "https://dashboard.example.com",
	}

	plan := config.PurchasePlan{
		ID:   "plan-123",
		Name: "Test Plan",
	}

	scheduledDate := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	execution := &config.PurchaseExecution{
		ExecutionID: "exec-456",
		// ApprovalToken deliberately holds a HASH-shaped value, not the raw
		// token under test: buildNotificationData must source the email's
		// ApprovalToken from the explicit rawApprovalToken parameter
		// (issue #103), never from this field.
		ApprovalToken:    config.HashApprovalToken("token-abc"),
		EstimatedSavings: 500.0,
		TotalUpfrontCost: 1500.0,
		ScheduledDate:    scheduledDate,
		Recommendations: []config.RecommendationRecord{
			{
				Service:      "rds",
				ResourceType: "db.r5.large",
				Engine:       "postgres",
				Region:       "us-east-1",
				Count:        2,
				Savings:      200.0,
			},
		},
	}

	data := manager.buildNotificationData(plan, execution, "token-abc", 5, "notify@example.com")

	assert.Equal(t, "https://dashboard.example.com", data.DashboardURL)
	assert.Equal(t, "token-abc", data.ApprovalToken)
	assert.Equal(t, 500.0, data.TotalSavings)
	assert.Equal(t, 1500.0, data.TotalUpfrontCost)
	assert.Equal(t, "February 1, 2024", data.PurchaseDate)
	assert.Equal(t, 5, data.DaysUntilPurchase)
	assert.Equal(t, "Test Plan", data.PlanName)
	assert.Len(t, data.Recommendations, 1)
	assert.Equal(t, "rds", data.Recommendations[0].Service)
}

func TestManager_GetOrCreateExecution(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)

	nextExec := time.Now().Add(24 * time.Hour)
	plan := &config.PurchasePlan{
		ID:      "plan-123",
		Name:    "Test Plan",
		Enabled: true, AutoPurchase: true,
		RampSchedule: config.RampSchedule{
			CurrentStep: 1,
		},
		NextExecutionDate: &nextExec,
	}

	manager := &Manager{
		config:       mockStore,
		email:        mockEmail,
		dashboardURL: "https://dashboard.example.com",
	}

	// No existing execution and no recommendation source: fail loud, write nothing.
	expectStepRows(mockStore, plan)

	manager.resolver = nil
	execution, rawToken, _, err := manager.getOrCreateExecution(ctx, plan)
	require.ErrorIs(t, err, errNoRecommendationSource)
	assert.Nil(t, execution)
	assert.Empty(t, rawToken)
	mockStore.AssertNotCalled(t, "SavePurchaseExecution", mock.Anything, mock.Anything)
	mockStore.AssertExpectations(t)
}

func TestManager_GetOrCreateExecution_ExistingExecution(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)

	nextExec := time.Now().Add(24 * time.Hour)
	plan := &config.PurchasePlan{
		ID:      "plan-123",
		Name:    "Test Plan",
		Enabled: true, AutoPurchase: true,
		RampSchedule: config.RampSchedule{
			CurrentStep: 1,
		},
		NextExecutionDate: &nextExec,
	}

	existingExec := &config.PurchaseExecution{
		ExecutionID: "existing-exec-id",
		PlanID:      "plan-123",
		Status:      "pending",
		// A hash-shaped placeholder: the whole point of the rotation branch
		// is that this value is never recoverable as a raw token, so
		// getOrCreateExecution must mint a fresh one rather than reuse it.
		ApprovalToken:   config.HashApprovalToken("stale-notified-token"),
		ScheduledDate:   nextExec,
		Recommendations: []config.RecommendationRecord{{Provider: "aws", Service: "ec2", Count: 1}},
	}

	// Existing execution found: getOrCreateExecution mints a fresh token into
	// the returned copy (issue #103 -- a stored hash can never be re-emailed
	// raw) with the full ApprovalTokenTTL, but persists nothing: no
	// SavePurchaseExecution expectation, so a write here panics the mock.
	expectStepRows(mockStore, plan, *existingExec)

	manager := &Manager{
		config:       mockStore,
		email:        mockEmail,
		dashboardURL: "https://dashboard.example.com",
	}

	execution, rawToken, rotationPending, err := manager.getOrCreateExecution(ctx, plan)
	require.NoError(t, err)
	assert.True(t, rotationPending, "the caller must persist the minted token after sending")
	assert.NotNil(t, execution)
	assert.Equal(t, "existing-exec-id", execution.ExecutionID)
	assert.Equal(t, "plan-123", execution.PlanID)
	// The rotated token must differ from the stale placeholder and hash to
	// the freshly returned raw value.
	assert.NotEmpty(t, rawToken)
	assert.NotEqual(t, config.HashApprovalToken("stale-notified-token"), execution.ApprovalToken)
	assert.Equal(t, config.HashApprovalToken(rawToken), execution.ApprovalToken)
	require.NotNil(t, execution.ApprovalTokenExpiresAt)
	assert.WithinDuration(t, time.Now().Add(config.ApprovalTokenTTL), *execution.ApprovalTokenExpiresAt, time.Minute)

	mockStore.AssertExpectations(t)
}

// A pending row that predates the #609 guard and carries no recommendations is
// failed at the tick instead of being rotated and emailed as a $0 approval.
func TestManager_GetOrCreateExecution_ExistingBareRowIsFailedNotRotated(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })

	nextExec := time.Now().Add(24 * time.Hour)
	pastDeadline := time.Now().Add(-10 * 24 * time.Hour)
	plan := &config.PurchasePlan{ID: "plan-123", Enabled: true, AutoPurchase: true, NotificationDaysBefore: 3, NextExecutionDate: &pastDeadline}
	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{}, nil)
	bare := &config.PurchaseExecution{
		ExecutionID:   "bare-exec-id",
		PlanID:        "plan-123",
		Status:        "pending",
		StepNumber:    1,
		ScheduledDate: nextExec,
	}
	expectStepRows(mockStore, plan, *bare)
	failed := *bare
	failed.Status = "failed"
	mockStore.On("TransitionExecutionStatus", ctx, "bare-exec-id", []string{"pending", "notified"}, "failed", (*string)(nil)).Return(&failed, nil)
	mockStore.On("SavePurchaseExecution", ctx, &failed).Return(nil)

	manager := &Manager{config: mockStore, resolver: &fakeResolver{err: planstep.ErrNoMatch}}

	execution, rawToken, rotationPending, err := manager.getOrCreateExecution(ctx, plan)
	require.ErrorIs(t, err, errExecutionNotNotifiable)
	assert.Nil(t, execution)
	assert.Empty(t, rawToken)
	assert.False(t, rotationPending)
	assert.Equal(t, "failed", failed.Status)
	assert.Contains(t, failed.Error, "no current recommendation matched")
}

// A human approving the row between the tick's read and its fail loses nothing:
// the CAS is refused, nothing is saved over the approval, and the tick skips.
func TestManager_GetOrCreateExecution_ExistingBareRowApprovedMeanwhileIsNotOverwritten(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })

	nextExec := time.Now().Add(24 * time.Hour)
	pastDeadline := time.Now().Add(-10 * 24 * time.Hour)
	plan := &config.PurchasePlan{ID: "plan-123", Enabled: true, AutoPurchase: true, NotificationDaysBefore: 3, NextExecutionDate: &pastDeadline}
	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{}, nil)
	stale := &config.PurchaseExecution{
		ExecutionID: "bare-exec-id", PlanID: "plan-123", Status: "pending", StepNumber: 1, ScheduledDate: nextExec,
	}
	expectStepRows(mockStore, plan, *stale)
	mockStore.On("TransitionExecutionStatus", ctx, "bare-exec-id", []string{"pending", "notified"}, "failed", (*string)(nil)).
		Return(nil, config.ErrExecutionNotInExpectedStatus)
	// No SavePurchaseExecution expectation: an upsert here would clobber the approval.

	manager := &Manager{config: mockStore, resolver: &fakeResolver{err: planstep.ErrNoMatch}}

	execution, _, _, err := manager.getOrCreateExecution(ctx, plan)
	require.ErrorIs(t, err, errExecutionNotNotifiable)
	assert.Nil(t, execution)
	mockStore.AssertNotCalled(t, "SavePurchaseExecution", mock.Anything, mock.Anything)
}

// TestManager_GetOrCreateExecution_ExistingCompletedNotRotated is the
// regression guard for a CodeRabbit finding on PR #416 (issue #103):
// GetExecutionByPlanAndDate filters only on plan_id + scheduled_date, not
// status, so a later notification tick for the same plan+date (e.g. before
// NextExecutionDate has advanced) can find a row that has already been
// approved, completed, or canceled since the last tick. Rotating its
// approval token unconditionally would overwrite the hash (and expiry) a
// still-live "purchase executed, click to revoke" email already committed
// to, 403-ing that link for no reason connected to the reminder-notification
// feature at all. Only pending/notified rows may be rotated.
func TestManager_GetOrCreateExecution_ExistingCompletedNotRotated(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)

	nextExec := time.Now().Add(24 * time.Hour)
	plan := &config.PurchasePlan{
		ID:                "plan-123",
		Name:              "Test Plan",
		Enabled:           true,
		AutoPurchase:      true,
		NextExecutionDate: &nextExec,
	}

	completedExec := &config.PurchaseExecution{
		ExecutionID: "completed-exec-id",
		PlanID:      "plan-123",
		Status:      "completed",
		// This is the hash of the LIVE revocation token already emailed in
		// the "purchase executed" notification. If getOrCreateExecution
		// rotated it, that email's Revoke link would 403.
		ApprovalToken: config.HashApprovalToken("live-revocation-token"),
		ScheduledDate: nextExec,
	}
	expectStepRows(mockStore, plan, *completedExec)
	// No SavePurchaseExecution / GetExecutionByID expectation: a completed
	// row must never be written to by the reminder-notification path.

	manager := &Manager{
		config:       mockStore,
		email:        mockEmail,
		dashboardURL: "https://dashboard.example.com",
	}

	execution, rawToken, _, err := manager.getOrCreateExecution(ctx, plan)
	require.Error(t, err, "a completed row must not be silently re-notified or rotated")
	assert.ErrorIs(t, err, errExecutionNotNotifiable)
	assert.Contains(t, err.Error(), "completed")
	assert.Nil(t, execution)
	assert.Empty(t, rawToken)
	// The stored hash for the live revocation link must be untouched.
	assert.Equal(t, config.HashApprovalToken("live-revocation-token"), completedExec.ApprovalToken)

	mockStore.AssertExpectations(t)
}

func TestManager_GetOrCreateExecution_SaveError(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)

	nextExec := time.Now().Add(-10 * 24 * time.Hour)
	plan := &config.PurchasePlan{
		ID:                     "plan-123",
		Name:                   "Test Plan",
		Enabled:                true,
		AutoPurchase:           true,
		NotificationDaysBefore: 3,
		NextExecutionDate:      &nextExec,
	}

	// No existing execution found; the failed row's save fails.
	expectStepRows(mockStore, plan)
	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{}, nil)
	mockStore.On("SavePurchaseExecution", ctx, mock.AnythingOfType("*config.PurchaseExecution")).Return(errors.New("save failed"))

	manager := &Manager{
		resolver:     &fakeResolver{err: planstep.ErrNoMatch},
		config:       mockStore,
		email:        mockEmail,
		dashboardURL: "https://dashboard.example.com",
	}

	execution, _, _, err := manager.getOrCreateExecution(ctx, plan)
	assert.Error(t, err)
	assert.NotErrorIs(t, err, errExecutionNotNotifiable, "a failed save must not read as a quiet skip")
	assert.Nil(t, execution)

	mockStore.AssertExpectations(t)
}

func TestManager_GetOrCreateExecution_LookupError(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)

	nextExec := time.Now().Add(24 * time.Hour)
	plan := &config.PurchasePlan{
		ID:                "plan-123",
		Name:              "Test Plan",
		Enabled:           true,
		AutoPurchase:      true,
		NextExecutionDate: &nextExec,
	}

	// Error looking up existing execution
	mockStore.On("LockPurchasePlanTx", mock.Anything, mock.Anything, plan.ID).Return(plan, nil)
	mockStore.On("ListExecutionsForPlanStepTx", mock.Anything, mock.Anything, plan.ID, plan.RampSchedule.CurrentStep+1).Return(nil, errors.New("db error"))

	manager := &Manager{
		config:       mockStore,
		email:        mockEmail,
		dashboardURL: "https://dashboard.example.com",
	}

	execution, _, _, err := manager.getOrCreateExecution(ctx, plan)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to check for existing execution")
	assert.Nil(t, execution)

	mockStore.AssertExpectations(t)
}

// TestManager_GetOrCreateExecution_CreatesOnErrNotFound is the F2 regression
// guard: when GetExecutionByPlanAndDate wraps ErrNotFound (zero rows), the
// create branch must fire rather than treating it as a hard error.
// Pre-fix, the store returned a plain fmt.Errorf on zero rows, so getOrCreateExecution
// treated it as a fatal error and the create branch was unreachable.
func TestManager_GetOrCreateExecution_CreatesOnErrNotFound(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)

	nextExec := time.Now().Add(-10 * 24 * time.Hour)
	plan := &config.PurchasePlan{
		ID:                     "plan-f2",
		Name:                   "F2 Plan",
		Enabled:                true,
		AutoPurchase:           true,
		NotificationDaysBefore: 3,
		NextExecutionDate:      &nextExec,
		RampSchedule:           config.RampSchedule{CurrentStep: 2},
	}

	// Store returns ErrNotFound (wrapped), matching the post-fix store behavior.
	expectStepRows(mockStore, plan)
	var saved *config.PurchaseExecution
	mockStore.On("SavePurchaseExecution", ctx, mock.AnythingOfType("*config.PurchaseExecution")).
		Run(func(args mock.Arguments) { saved = args.Get(1).(*config.PurchaseExecution) }).Return(nil)

	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{}, nil)
	manager := &Manager{config: mockStore, dashboardURL: "https://example.com", resolver: &fakeResolver{err: planstep.ErrNoMatch}}

	execution, _, _, err := manager.getOrCreateExecution(ctx, plan)
	require.ErrorIs(t, err, errExecutionNotNotifiable, "ErrNotFound must trigger the create path, not a hard error (F2)")
	assert.Nil(t, execution)
	require.NotNil(t, saved)
	assert.Equal(t, "plan-f2", saved.PlanID)
	assert.Equal(t, "failed", saved.Status)
	// Two steps already completed, so this row is step 3 (issue #1669).
	assert.Equal(t, 3, saved.StepNumber)
	assert.NotEmpty(t, saved.ExecutionID)

	mockStore.AssertExpectations(t)
}

func TestManager_SendUpcomingPurchaseNotifications_WithNotification(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)

	nextExec := time.Now().Add(3 * 24 * time.Hour) // 3 days from now
	plans := []config.PurchasePlan{
		{
			ID:                     "plan-123",
			Name:                   "Test Plan",
			Enabled:                true,
			AutoPurchase:           true,
			NotificationDaysBefore: 7,
			NextExecutionDate:      &nextExec,
			LastNotificationSent:   nil,
			RampSchedule:           config.RampSchedule{CurrentStep: 0},
		},
	}

	notifyEmailStr := "notify@example.com"
	globalCfg := &config.GlobalConfig{NotificationEmail: &notifyEmailStr, AutoPurchaseEnabled: true}
	mockStore.On("ListPurchasePlans", ctx, config.PurchasePlanFilter{}).Return(plans, nil)
	// An existing pending row that carries recommendations is the only kind
	// the reminder still emails (#609); it is rotated, not created.
	pendingExec := &config.PurchaseExecution{
		ExecutionID:     "pending-exec-id",
		PlanID:          "plan-123",
		Status:          "pending",
		StepNumber:      1,
		ScheduledDate:   nextExec,
		Recommendations: []config.RecommendationRecord{{Provider: "aws", Service: "ec2", Count: 1}},
	}
	expectStepRows(mockStore, &plans[0], *pendingExec)
	mockStore.On("RotatePendingApprovalToken", ctx, "pending-exec-id", mock.Anything, mock.Anything).Return(true, nil)
	mockStore.On("GetGlobalConfig", ctx).Return(globalCfg, nil)
	mockEmail.On("SendScheduledPurchaseNotification", ctx, mock.AnythingOfType("email.NotificationData")).Return(nil)
	mockStore.On("StampPlanNotificationSent", ctx, "plan-123", mock.AnythingOfType("time.Time")).Return(nil)

	manager := &Manager{
		config:       mockStore,
		email:        mockEmail,
		notifyDays:   7,
		dashboardURL: "https://dashboard.example.com",
	}

	result, err := manager.SendUpcomingPurchaseNotifications(ctx)
	require.NoError(t, err)

	assert.Equal(t, 1, result.Notified)

	mockStore.AssertExpectations(t)
	mockEmail.AssertExpectations(t)
}

func TestManager_SendUpcomingPurchaseNotifications_TooFarAway(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)

	nextExec := time.Now().Add(14 * 24 * time.Hour) // 14 days from now
	plans := []config.PurchasePlan{
		{
			ID:                     "plan-123",
			Name:                   "Test Plan",
			Enabled:                true,
			AutoPurchase:           true,
			NotificationDaysBefore: 7,
			NextExecutionDate:      &nextExec,
		},
	}

	mockStore.On("ListPurchasePlans", ctx, config.PurchasePlanFilter{}).Return(plans, nil)

	manager := &Manager{
		config:       mockStore,
		email:        mockEmail,
		notifyDays:   7,
		dashboardURL: "https://dashboard.example.com",
	}

	result, err := manager.SendUpcomingPurchaseNotifications(ctx)
	require.NoError(t, err)

	assert.Equal(t, 0, result.Notified)

	mockStore.AssertExpectations(t)
}

func TestManager_SendUpcomingPurchaseNotifications_RecentNotification(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)

	nextExec := time.Now().Add(3 * 24 * time.Hour) // 3 days from now
	lastNotif := time.Now().Add(-12 * time.Hour)   // 12 hours ago
	plans := []config.PurchasePlan{
		{
			ID:                     "plan-123",
			Name:                   "Test Plan",
			Enabled:                true,
			AutoPurchase:           true,
			NotificationDaysBefore: 7,
			NextExecutionDate:      &nextExec,
			LastNotificationSent:   &lastNotif,
		},
	}

	mockStore.On("ListPurchasePlans", ctx, config.PurchasePlanFilter{}).Return(plans, nil)

	manager := &Manager{
		config:       mockStore,
		email:        mockEmail,
		notifyDays:   7,
		dashboardURL: "https://dashboard.example.com",
	}

	result, err := manager.SendUpcomingPurchaseNotifications(ctx)
	require.NoError(t, err)

	assert.Equal(t, 0, result.Notified)

	mockStore.AssertExpectations(t)
}

func TestManager_SendUpcomingPurchaseNotifications_NoNextExecutionDate(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)

	plans := []config.PurchasePlan{
		{
			ID:                     "plan-123",
			Name:                   "Test Plan",
			Enabled:                true,
			AutoPurchase:           true,
			NotificationDaysBefore: 7,
			NextExecutionDate:      nil, // No next execution date
		},
	}

	mockStore.On("ListPurchasePlans", ctx, config.PurchasePlanFilter{}).Return(plans, nil)

	manager := &Manager{
		config:       mockStore,
		email:        mockEmail,
		notifyDays:   7,
		dashboardURL: "https://dashboard.example.com",
	}

	result, err := manager.SendUpcomingPurchaseNotifications(ctx)
	require.NoError(t, err)

	assert.Equal(t, 0, result.Notified)

	mockStore.AssertExpectations(t)
}

func TestManager_SendUpcomingPurchaseNotifications_EmailFails(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)

	nextExec := time.Now().Add(3 * 24 * time.Hour)
	plans := []config.PurchasePlan{
		{
			ID:                     "plan-123",
			Name:                   "Test Plan",
			Enabled:                true,
			AutoPurchase:           true,
			NotificationDaysBefore: 7,
			NextExecutionDate:      &nextExec,
			RampSchedule:           config.RampSchedule{CurrentStep: 0},
		},
	}

	notifyEmailStr := "notify@example.com"
	globalCfg := &config.GlobalConfig{NotificationEmail: &notifyEmailStr, AutoPurchaseEnabled: true}
	mockStore.On("ListPurchasePlans", ctx, config.PurchasePlanFilter{}).Return(plans, nil)
	pendingExec := &config.PurchaseExecution{
		ExecutionID:     "pending-exec-id",
		PlanID:          "plan-123",
		Status:          "pending",
		StepNumber:      1,
		ScheduledDate:   nextExec,
		Recommendations: []config.RecommendationRecord{{Provider: "aws", Service: "ec2", Count: 1}},
	}
	expectStepRows(mockStore, &plans[0], *pendingExec)
	mockStore.On("GetGlobalConfig", ctx).Return(globalCfg, nil)
	mockEmail.On("SendScheduledPurchaseNotification", ctx, mock.AnythingOfType("email.NotificationData")).Return(errors.New("email failed"))

	manager := &Manager{
		config:       mockStore,
		email:        mockEmail,
		notifyDays:   7,
		dashboardURL: "https://dashboard.example.com",
	}

	result, err := manager.SendUpcomingPurchaseNotifications(ctx)
	require.NoError(t, err)

	assert.Equal(t, 0, result.Notified)

	mockStore.AssertExpectations(t)
	mockEmail.AssertExpectations(t)
}

// pendingExecForNotification sets up a plan due for a notification whose
// execution row already exists as "pending" with the hash of the token the
// previous notification emailed.
func pendingExecForNotification(mockStore *MockConfigStore) (*config.PurchasePlan, *config.PurchaseExecution) {
	nextExec := time.Now().Add(3 * 24 * time.Hour)
	plan := &config.PurchasePlan{ID: "plan-rot", Name: "Rotation Plan", Enabled: true, AutoPurchase: true, NextExecutionDate: &nextExec}
	existing := &config.PurchaseExecution{
		ExecutionID:     "exec-rot",
		PlanID:          "plan-rot",
		Status:          "pending",
		ApprovalToken:   config.HashApprovalToken("previously-emailed-token"),
		ScheduledDate:   nextExec,
		Recommendations: []config.RecommendationRecord{{Provider: "aws", Service: "ec2", Count: 1}},
	}
	mockStore.On("LockPurchasePlanTx", mock.Anything, mock.Anything, plan.ID).Return(plan, nil).Maybe()
	mockStore.On("ListExecutionsForPlanStepTx", mock.Anything, mock.Anything, plan.ID, plan.RampSchedule.CurrentStep+1).
		Return([]config.PurchaseExecution{*existing}, nil).Maybe()
	return plan, existing
}

// TestManager_SendPlanNotification_NoRecipientLeavesLiveTokenAlone pins the
// #416 review finding: without a notification email nothing is sent, so the
// previously emailed link must stay live. No write expectation is registered,
// so any token persist panics the mock.
func TestManager_SendPlanNotification_NoRecipientLeavesLiveTokenAlone(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)
	plan, existing := pendingExecForNotification(mockStore)
	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{}, nil)

	manager := &Manager{config: mockStore, email: mockEmail, dashboardURL: "https://dashboard.example.com"}
	assert.False(t, manager.sendPlanNotification(ctx, plan))
	assert.Equal(t, config.HashApprovalToken("previously-emailed-token"), existing.ApprovalToken)
	mockStore.AssertExpectations(t)
	mockEmail.AssertExpectations(t)
}

// TestManager_SendPlanNotification_SendFailureLeavesLiveTokenAlone: an SES
// failure must not persist the replacement token, or the previous email's
// link dies with no working successor.
func TestManager_SendPlanNotification_SendFailureLeavesLiveTokenAlone(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)
	plan, _ := pendingExecForNotification(mockStore)
	notify := "notify@example.com"
	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{NotificationEmail: &notify}, nil)
	mockEmail.On("SendScheduledPurchaseNotification", ctx, mock.AnythingOfType("email.NotificationData")).Return(errors.New("ses down"))

	manager := &Manager{config: mockStore, email: mockEmail, dashboardURL: "https://dashboard.example.com"}
	assert.False(t, manager.sendPlanNotification(ctx, plan))
	mockStore.AssertNotCalled(t, "RotatePendingApprovalToken", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	mockStore.AssertNotCalled(t, "StampPlanNotificationSent", mock.Anything, mock.Anything, mock.Anything)
	mockStore.AssertExpectations(t)
	mockEmail.AssertExpectations(t)
}

// TestManager_SendPlanNotification_PersistsEmailedTokenHashAfterSend: on a
// successful send, the hash persisted (via the status-guarded targeted
// update, never a full-row upsert) is the hash of exactly the raw token that
// was emailed, and the raw token itself is never stored.
func TestManager_SendPlanNotification_PersistsEmailedTokenHashAfterSend(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)
	plan, _ := pendingExecForNotification(mockStore)
	notify := "notify@example.com"
	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{NotificationEmail: &notify}, nil)

	var emailedToken string
	mockEmail.On("SendScheduledPurchaseNotification", ctx, mock.AnythingOfType("email.NotificationData")).
		Run(func(args mock.Arguments) {
			emailedToken = args.Get(1).(email.NotificationData).ApprovalToken
		}).Return(nil)
	var persistedHash string
	mockStore.On("RotatePendingApprovalToken", ctx, "exec-rot", mock.AnythingOfType("string"), mock.AnythingOfType("time.Time")).
		Run(func(args mock.Arguments) {
			require.NotEmpty(t, emailedToken, "the token must be persisted only after the email went out")
			persistedHash = args.String(2)
		}).Return(true, nil)
	mockStore.On("StampPlanNotificationSent", ctx, plan.ID, mock.AnythingOfType("time.Time")).Return(nil)

	manager := &Manager{config: mockStore, email: mockEmail, dashboardURL: "https://dashboard.example.com"}
	assert.True(t, manager.sendPlanNotification(ctx, plan))
	require.NotEmpty(t, emailedToken)
	assert.Equal(t, config.HashApprovalToken(emailedToken), persistedHash)
	assert.NotEqual(t, emailedToken, persistedHash)
	mockStore.AssertExpectations(t)
	mockEmail.AssertExpectations(t)
}

func TestManager_SendPlanNotification_StampFailure(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	sender := new(MockEmailSender)
	plan, _ := pendingExecForNotification(store)
	recipient := "fixture@example.invalid"
	store.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{NotificationEmail: &recipient}, nil)
	sender.On("SendScheduledPurchaseNotification", ctx, mock.Anything).Return(nil)
	store.On("RotatePendingApprovalToken", ctx, "exec-rot", mock.Anything, mock.Anything).Return(true, nil)
	store.On("StampPlanNotificationSent", ctx, plan.ID, mock.Anything).Return(errors.New("stamp failed"))
	manager := &Manager{config: store, email: sender}
	require.False(t, manager.sendPlanNotification(ctx, plan))
	require.Nil(t, plan.LastNotificationSent)
	store.AssertExpectations(t)
	sender.AssertExpectations(t)
}
