package purchase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Fixtures below are MockConfigStore expectations and hand-built records, not
// real cloud data. A MockProviderFactory with no expectations panics on any
// provider call, so "no provider call" is asserted by the absence of a panic
// plus an explicit AssertNotCalled.

var errPinLost = errors.New("pin lost")

// newPinManager wires store for one due plan row on the cron and SQS paths. The
// plan the gate reads is gatePlan; every later GetPurchasePlan read (the
// post-claim re-check) returns claimPlan.
func newPinManager(ctx context.Context, store *MockConfigStore, factory *MockProviderFactory, exec config.PurchaseExecution, gatePlan, claimPlan *config.PurchasePlan) *Manager {
	store.On("GetStaleApprovedExecutions", ctx, mock.Anything).Return([]config.PurchaseExecution{}, nil)
	store.On("GetPendingExecutions", ctx).Return([]config.PurchaseExecution{exec}, nil)
	store.On("GetExecutionByID", ctx, exec.ExecutionID).Return(&exec, nil)
	store.On("GetPurchasePlan", ctx, exec.PlanID).Return(gatePlan, nil).Once()
	store.On("GetPurchasePlan", ctx, exec.PlanID).Return(claimPlan, nil)
	store.On("GetGlobalConfig", ctx).Return(autoPurchaseOnCfg(), nil)
	// The purchase-failed notice (#714) reads the recipient on a detached context.
	store.On("GetGlobalConfig", mock.Anything).Return(autoPurchaseOnCfg(), nil).Maybe()
	return &Manager{config: store, email: allowFailedNotification(new(MockEmailSender)), providerFactory: factory}
}

func autoPlan(c *float64) *config.PurchasePlan {
	return &config.PurchasePlan{ID: "plan-gate", AutoPurchase: true, MaxStepCommitment: c}
}

// S2: an attach landing between the gate (which saw an empty row) and the claim
// makes the pinned claim lose; nothing runs, on both the cron and SQS paths.
func TestClaimPin_AttachBetweenGateAndClaimIsRefused(t *testing.T) {
	ctx := context.Background()
	for _, path := range []string{"cron", "sqs"} {
		t.Run(path, func(t *testing.T) {
			store, factory := new(MockConfigStore), new(MockProviderFactory)
			manager := newPinManager(ctx, store, factory, dueRecExec("notified"), autoPlan(f64(1000)), autoPlan(f64(1000)))
			store.On("TransitionExecutionStatusPinned", ctx, "exec-gate", []string{"approved", "pending", "notified"}, "running", (*string)(nil), true).
				Return(nil, config.ErrExecutionNotInExpectedStatus)

			if path == "cron" {
				result, err := manager.ProcessScheduledPurchases(ctx)
				require.NoError(t, err)
				assert.Equal(t, 0, result.Executed)
				assert.Equal(t, 0, result.Failed, "a lost pin is a benign skip")
			} else {
				require.NoError(t, manager.handleExecutePurchase(ctx, &AsyncMessage{Type: MessageTypeExecutePurchase, ExecutionID: "exec-gate"}))
			}

			store.AssertCalled(t, "TransitionExecutionStatusPinned", ctx, "exec-gate", []string{"approved", "pending", "notified"}, "running", (*string)(nil), true)
			store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			store.AssertNotCalled(t, "SavePurchaseExecution", mock.Anything, mock.Anything)
			factory.AssertNotCalled(t, "CreateAndValidateProvider", mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

// A row the gate saw WITH recommendations is claimed with the opposite pin.
func TestClaimPin_RecommendationBearingRowPinsNonEmpty(t *testing.T) {
	ctx := context.Background()
	store, factory := new(MockConfigStore), new(MockProviderFactory)
	manager := newPinManager(ctx, store, factory, dueRecExec("notified", upfrontRec(10)), autoPlan(f64(1000)), autoPlan(f64(1000)))
	store.On("TransitionExecutionStatusPinned", ctx, "exec-gate", mock.Anything, "running", (*string)(nil), false).
		Return(nil, config.ErrExecutionNotInExpectedStatus)
	result, err := manager.ProcessScheduledPurchases(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, result.Failed)
	store.AssertCalled(t, "TransitionExecutionStatusPinned", ctx, "exec-gate", mock.Anything, "running", (*string)(nil), false)
	factory.AssertNotCalled(t, "CreateAndValidateProvider", mock.Anything, mock.Anything, mock.Anything)
}

// A real error from the pinned claim is a failure, not a skip.
func TestClaimPin_ClaimErrorIsFailure(t *testing.T) {
	ctx := context.Background()
	store, factory := new(MockConfigStore), new(MockProviderFactory)
	manager := newPinManager(ctx, store, factory, dueRecExec("notified"), autoPlan(f64(1000)), autoPlan(f64(1000)))
	store.On("TransitionExecutionStatusPinned", ctx, "exec-gate", mock.Anything, "running", (*string)(nil), true).Return(nil, errPinLost)
	result, err := manager.ProcessScheduledPurchases(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Failed)
}

// S2: the cap is re-applied to the claimed row against the plan as it is now.
// The gate saw cap 1000 and the plan was then lowered to 5: the claimed row is
// failed with the beforeSubmission class (suppressions released), no provider
// call is made.
func TestClaimPin_PostClaimCapRecheckFailsBeforeSubmission(t *testing.T) {
	ctx := context.Background()
	for _, path := range []string{"cron", "sqs"} {
		t.Run(path, func(t *testing.T) {
			exec := dueRecExec("notified", upfrontRec(10))
			exec.Recommendations[0].ID = "rec-1"
			store, factory := new(MockConfigStore), new(MockProviderFactory)
			manager := newPinManager(ctx, store, factory, exec, autoPlan(f64(1000)), autoPlan(f64(5)))
			claimed := exec
			claimed.Status = "running"
			store.On("TransitionExecutionStatusPinned", ctx, "exec-gate", mock.Anything, "running", (*string)(nil), false).Return(&claimed, nil)
			var saved *config.PurchaseExecution
			store.On("SaveExecutionIfStatusTx", ctx, mock.Anything, mock.Anything, []string{"running"}).
				Run(func(args mock.Arguments) { saved = args.Get(2).(*config.PurchaseExecution) }).Return(nil)
			store.On("DeleteSuppressionsByExecutionTx", ctx, mock.Anything, "exec-gate").Return(nil)

			if path == "cron" {
				result, err := manager.ProcessScheduledPurchases(ctx)
				require.NoError(t, err)
				assert.Equal(t, 1, result.Failed)
				assert.Equal(t, 0, result.Executed)
			} else {
				err := manager.handleExecutePurchase(ctx, &AsyncMessage{Type: MessageTypeExecutePurchase, ExecutionID: "exec-gate"})
				require.ErrorIs(t, err, ErrStepCommitmentCap)
			}

			require.NotNil(t, saved)
			assert.Equal(t, "failed", saved.Status)
			assert.Contains(t, saved.Error, "exceeds max_step_commitment")
			store.AssertCalled(t, "DeleteSuppressionsByExecutionTx", ctx, mock.Anything, "exec-gate")
			factory.AssertNotCalled(t, "CreateAndValidateProvider", mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

// A post-claim plan read failure also fails the claimed row closed rather than
// running it unchecked.
func TestClaimPin_PostClaimPlanReadErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	exec := dueRecExec("notified", upfrontRec(10))
	store := new(MockConfigStore)
	store.On("GetPurchasePlan", ctx, exec.PlanID).Return(nil, errors.New("db down"))
	claimed := exec
	claimed.Status = "running"
	store.On("TransitionExecutionStatusPinned", ctx, exec.ExecutionID, mock.Anything, "running", (*string)(nil), false).Return(&claimed, nil)
	var saved *config.PurchaseExecution
	store.On("SavePurchaseExecutionTx", ctx, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { saved = args.Get(2).(*config.PurchaseExecution) }).Return(nil)
	sender := allowFailedNotification(new(MockEmailSender))
	manager := &Manager{config: store, email: sender, providerFactory: new(MockProviderFactory)}

	didClaim, err := manager.claimAndExecute(ctx, &exec)
	require.True(t, didClaim)
	require.Error(t, err)
	assert.ErrorContains(t, err, "post-claim cap check")
	require.NotNil(t, saved)
	assert.Equal(t, "failed", saved.Status)
	sender.AssertNumberOfCalls(t, "SendPurchaseFailedNotification", 1) // issue #1195
	store.AssertCalled(t, "DeleteSuppressionsByExecutionTx", ctx, mock.Anything, exec.ExecutionID)
}

// Manual / approved purchases are unchanged: an approved row is claimed and run
// with no cap re-check, even on a plan that has no cap (the cap error class
// never appears and the plan is read once, by executePurchase itself).
func TestClaimPin_ApprovedRowSkipsCapRecheck(t *testing.T) {
	ctx := context.Background()
	exec := dueRecExec("approved", upfrontRec(1e9))
	store := new(MockConfigStore)
	store.On("GetPurchasePlan", mock.Anything, exec.PlanID).Return(autoPlan(nil), nil)
	store.On("GetPlanAccounts", mock.Anything, exec.PlanID).Return(nil, errors.New("accounts unavailable"))
	claimed := exec
	claimed.Status = "running"
	store.On("TransitionExecutionStatusPinned", ctx, exec.ExecutionID, mock.Anything, "running", (*string)(nil), false).Return(&claimed, nil)
	store.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(nil)
	manager := &Manager{config: store, email: allowFailedNotification(new(MockEmailSender)), providerFactory: new(MockProviderFactory)}

	didClaim, err := manager.claimAndExecute(ctx, &exec)
	require.True(t, didClaim)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrStepCommitmentCap)
	store.AssertNumberOfCalls(t, "GetPurchasePlan", 1)
}

// S1: when the notification email fails nothing is promoted, and the scheduler
// then refuses the still-pending rec-bearing row (no claim, no failure).
func TestNotification_EmailFailureLeavesRowPendingAndSchedulerRefusesIt(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	sender := new(MockEmailSender)
	plan, existing := pendingExecForNotification(store)
	notify := "notify@example.com"
	store.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{NotificationEmail: &notify, AutoPurchaseEnabled: true}, nil)
	sender.On("SendScheduledPurchaseNotification", ctx, mock.Anything).Return(errors.New("ses down"))
	manager := &Manager{config: store, email: sender}

	require.False(t, manager.sendPlanNotification(ctx, plan))
	store.AssertNotCalled(t, "RotatePendingApprovalToken", mock.Anything, mock.Anything, mock.Anything, mock.Anything)

	// The row as the DB still holds it: pending. The scheduler leaves it alone.
	due := *existing
	due.ScheduledDate = time.Now().Add(-time.Hour)
	store.On("GetStaleApprovedExecutions", ctx, mock.Anything).Return([]config.PurchaseExecution{}, nil)
	store.On("GetPendingExecutions", ctx).Return([]config.PurchaseExecution{due}, nil)
	store.On("GetPurchasePlan", ctx, due.PlanID).Return(&config.PurchasePlan{ID: due.PlanID, AutoPurchase: true, MaxStepCommitment: f64(1000)}, nil)
	result, err := manager.ProcessScheduledPurchases(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, result.Processed)
	assert.Equal(t, 0, result.Failed)
	store.AssertNotCalled(t, "TransitionExecutionStatusPinned", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// Rows without recommendations keep their handling (S10): the gate lets them
// through to the claim, where executePurchase fails them. Unchanged by the pin.
func TestClaimPin_RecLessRowStillFailsInsteadOfBuying(t *testing.T) {
	ctx := context.Background()
	store, factory := new(MockConfigStore), new(MockProviderFactory)
	exec := dueRecExec("pending")
	manager := newPinManager(ctx, store, factory, exec, autoPlan(nil), autoPlan(nil))
	store.On("GetPurchasePlan", mock.Anything, exec.PlanID).Return(autoPlan(nil), nil)
	claimed := exec
	claimed.Status = "running"
	store.On("TransitionExecutionStatusPinned", ctx, "exec-gate", mock.Anything, "running", (*string)(nil), true).Return(&claimed, nil)
	var saved *config.PurchaseExecution
	store.On("SavePurchaseExecutionTx", mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { saved = args.Get(2).(*config.PurchaseExecution) }).Return(nil)
	result, err := manager.ProcessScheduledPurchases(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Failed)
	require.NotNil(t, saved)
	assert.Equal(t, "failed", saved.Status)
	assert.Contains(t, saved.Error, ErrPlanStepNoRecommendations.Error())
	factory.AssertNotCalled(t, "CreateAndValidateProvider", mock.Anything, mock.Anything, mock.Anything)
}
