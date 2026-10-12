package purchase

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// newFireManager builds a Manager wired with just enough mocks for the
// scheduled-fire sweep. The CAS-race and error paths exercised here return
// before executeAndFinalize, so the provider/email deps are intentionally nil
// (the same minimal-wiring approach reaper_test.go uses).
func newFireManager(store *MockConfigStore) *Manager {
	return &Manager{config: store}
}

// dueExec builds a representative status=scheduled execution whose
// scheduled_execution_at is in the past (i.e. due to fire). The SELECT in
// GetScheduledExecutionsDue is what enforces the due condition in production;
// the mock returns whatever the test wants.
func dueExec(id string) config.PurchaseExecution {
	past := time.Now().Add(-1 * time.Hour)
	return config.PurchaseExecution{
		PlanID:               "plan-1",
		ExecutionID:          id,
		Status:               "scheduled",
		ScheduledExecutionAt: &past,
	}
}

func TestFireScheduledDelayedPurchases_NoDueRows(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	store.On("GetScheduledExecutionsDue", ctx).
		Return([]config.PurchaseExecution{}, nil)

	mgr := newFireManager(store)
	result, err := mgr.FireScheduledDelayedPurchases(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, result.Found)
	assert.Equal(t, 0, result.Fired)
	assert.Equal(t, 0, result.RaceLost)
	assert.Equal(t, 0, result.Errored)
	store.AssertExpectations(t)
	// No CAS attempted when nothing is due. Named without matchers: that is
	// how this mock spells "never called at all, whatever the arguments".
	store.AssertNotCalled(t, "TransitionExecutionStatus")
}

func TestFireScheduledDelayedPurchases_ListErrorSurfaces(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	store.On("GetScheduledExecutionsDue", ctx).
		Return([]config.PurchaseExecution(nil), fmt.Errorf("db down"))

	mgr := newFireManager(store)
	result, err := mgr.FireScheduledDelayedPurchases(ctx)
	require.Error(t, err)
	assert.Nil(t, result)
	store.AssertExpectations(t)
}

func TestFireScheduledDelayedPurchases_CASLostToRevokeClassifiedAsRaceLost(t *testing.T) {
	// The critical safety property of the Gmail-style pre-fire delay: if the
	// user clicks Revoke between the sweep's SELECT and its scheduled->approved
	// CAS, the row is flipped to "cancelled" first and the CAS is rejected with
	// ErrExecutionNotInExpectedStatus. That MUST be classified as RaceLost (a
	// normal, expected outcome), NOT Errored — and the row must NOT fire the
	// SDK call. A regression here would either double-charge the user (fire a
	// purchase they revoked) or page ops on a benign race.
	ctx := context.Background()
	store := new(MockConfigStore)

	row := dueExec("exec-revoked")
	store.On("GetScheduledExecutionsDue", ctx).
		Return([]config.PurchaseExecution{row}, nil)
	store.On("TransitionExecutionStatus", ctx, "exec-revoked", []string{"scheduled"}, "running", (*string)(nil)).
		Return(nil, fmt.Errorf("%w: execution exec-revoked cannot transition from %q to %q",
			config.ErrExecutionNotInExpectedStatus, "cancelled", "approved"))

	mgr := newFireManager(store)
	result, err := mgr.FireScheduledDelayedPurchases(ctx)
	require.NoError(t, err) // the sweep itself succeeds even on a per-row race
	assert.Equal(t, 1, result.Found)
	assert.Equal(t, 0, result.Fired)
	assert.Equal(t, 1, result.RaceLost)
	assert.Equal(t, 0, result.Errored)
	store.AssertExpectations(t)
	// The revoke won: no audit stamp, no SDK fire.
	store.AssertNotCalled(t, "SavePurchaseExecution", mock.Anything, mock.Anything)
}

func TestFireScheduledDelayedPurchases_RowVanishedTreatedAsRaceLost(t *testing.T) {
	// Defensive: the row could disappear (cleanup / DBA action) between the
	// SELECT and the CAS. The store wraps that in config.ErrNotFound; the
	// sweep must treat it as RaceLost (nothing to fire) rather than a real
	// error.
	ctx := context.Background()
	store := new(MockConfigStore)

	row := dueExec("exec-gone")
	store.On("GetScheduledExecutionsDue", ctx).
		Return([]config.PurchaseExecution{row}, nil)
	store.On("TransitionExecutionStatus", ctx, "exec-gone", []string{"scheduled"}, "running", (*string)(nil)).
		Return(nil, fmt.Errorf("%w: execution exec-gone", config.ErrNotFound))

	mgr := newFireManager(store)
	result, err := mgr.FireScheduledDelayedPurchases(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Found)
	assert.Equal(t, 0, result.Fired)
	assert.Equal(t, 1, result.RaceLost)
	assert.Equal(t, 0, result.Errored)
	store.AssertExpectations(t)
	store.AssertNotCalled(t, "SavePurchaseExecution", mock.Anything, mock.Anything)
}

func TestFireScheduledDelayedPurchases_HardDBErrorClassifiedAsErrored(t *testing.T) {
	// A genuine DB failure on the CAS (connection reset, deadlock) is NOT a
	// race loss and must bump Errored so ops can see the outage — mirrors the
	// reaper's hard-error classification (the symmetric A1 CR finding).
	ctx := context.Background()
	store := new(MockConfigStore)

	row := dueExec("exec-dberr")
	store.On("GetScheduledExecutionsDue", ctx).
		Return([]config.PurchaseExecution{row}, nil)
	store.On("TransitionExecutionStatus", ctx, "exec-dberr", []string{"scheduled"}, "running", (*string)(nil)).
		Return(nil, fmt.Errorf("connection reset by peer"))

	mgr := newFireManager(store)
	result, err := mgr.FireScheduledDelayedPurchases(ctx)
	require.NoError(t, err) // wholesale-failure isolation: one bad row doesn't fail the sweep
	assert.Equal(t, 1, result.Found)
	assert.Equal(t, 0, result.Fired)
	assert.Equal(t, 0, result.RaceLost, "real DB errors must NOT be classified as race-lost")
	assert.Equal(t, 1, result.Errored, "real DB errors must bump Errored so ops can see the outage")
	store.AssertExpectations(t)
	store.AssertNotCalled(t, "SavePurchaseExecution", mock.Anything, mock.Anything)
}

// TestFireScheduledDelayedPurchases_EndToEnd exercises the full sequence:
// a purchase_execution in status=scheduled (purchase_delay_hours > 0) is found
// by GetScheduledExecutionsDue, the CAS transitions it to approved, the
// approved_by audit stamp is saved, and executeAndFinalize is invoked and
// actually fires a real cloud purchase.
//
// This is the A16-004 regression guard (#257). It was previously a t.Skip
// placeholder; every other test in this file asserts Fired == 0 (no-due-rows,
// list-error, CAS-race-lost, hard-DB-error), and the compile-time
// TestFireScheduledDelayedPurchases_DelayPathNotSilentNoOp only locks the
// method signature, so nothing observed result.Fired == 1 or a real
// PurchaseCommitment call. A change that made fireOneDue return (false, false)
// for every due row, or executeAndFinalize error out unconditionally, would
// silently stop every delayed purchase from ever firing while this suite
// stayed green.
//
// Uses the awsAccessKeyCredStore + MockProviderFactory chain already built
// for money_path_regression_test.go so the provider/credential wiring is
// real end to end and only the cloud SDK call itself is stubbed.
func TestFireScheduledDelayedPurchases_EndToEnd(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	mockEmail := new(MockEmailSender)
	mockFactory := new(MockProviderFactory)
	mockProviderInst := new(MockProvider)
	mockServiceClient := new(MockServiceClient)

	const acctID = "acct-fire"
	row := dueExec("exec-scheduled-fire")
	row.StepNumber = 1
	row.Recommendations = []config.RecommendationRecord{
		{
			Provider: "aws", Service: "ec2", ResourceType: "m5.large", Region: "us-east-1",
			Count: 1, UpfrontCost: 300, Savings: 60, Selected: true, CloudAccountID: aws.String(acctID),
		},
	}
	account := &config.CloudAccount{
		ID: acctID, Name: "Fire Account", Provider: "aws", ExternalID: "333333333333", AWSAuthMode: "access_keys",
	}
	plan := &config.PurchasePlan{ID: "plan-1", Name: "Plan 1", RampSchedule: config.RampSchedule{TotalSteps: 1}}

	store.On("GetScheduledExecutionsDue", ctx).Return([]config.PurchaseExecution{row}, nil)

	approved := row
	approved.Status = "approved"
	store.On("TransitionExecutionStatus", ctx, "exec-scheduled-fire", []string{"scheduled"}, "running", (*string)(nil)).
		Return(&approved, nil)

	store.On("GetPurchasePlan", ctx, "plan-1").Return(plan, nil)
	store.GetPlanAccountsFn = func(_ context.Context, _ string) ([]config.CloudAccount, error) {
		return nil, nil // no plan-level accounts -> single-account path
	}
	store.On("GetCloudAccount", ctx, acctID).Return(account, nil)
	store.On("CompletePlanStep", ctx, "plan-1", 1).Return(nil)

	var savedFinal *config.PurchaseExecution
	// The audit stamp and the terminal save are both guarded on running; an
	// unguarded SavePurchaseExecution would be an unexpected call here.
	store.On("SaveExecutionIfStatus", ctx, mock.Anything, []string{"running"}).
		Run(func(args mock.Arguments) {
			c := *args.Get(1).(*config.PurchaseExecution)
			savedFinal = &c
		}).Return(nil)
	store.On("SavePurchaseHistory", ctx, mock.AnythingOfType("*config.PurchaseHistoryRecord")).Return(nil)
	mockEmail.On("SendPurchaseConfirmation", ctx, mock.AnythingOfType("email.NotificationData")).Return(nil)

	mockFactory.On("CreateAndValidateProvider", mock.Anything, "aws", mock.Anything).Return(mockProviderInst, nil)
	mockProviderInst.On("GetServiceClient", mock.Anything, common.ServiceEC2, mock.Anything).Return(mockServiceClient, nil)
	mockServiceClient.On("PurchaseCommitment", mock.Anything, mock.Anything, mock.Anything).
		Return(common.PurchaseResult{Success: true, CommitmentID: "ri-fired"}, nil).Once()

	mgr := &Manager{
		config:          store,
		email:           mockEmail,
		providerFactory: mockFactory,
		credStore:       awsAccessKeyCredStore(),
		dashboardURL:    "https://dashboard.example.com",
	}

	result, err := mgr.FireScheduledDelayedPurchases(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Found)
	assert.Equal(t, 1, result.Fired, "a due scheduled purchase must actually fire")
	assert.Equal(t, 0, result.RaceLost)
	assert.Equal(t, 0, result.Errored)

	require.NotNil(t, savedFinal, "the fired execution must be persisted")
	assert.Equal(t, "completed", savedFinal.Status)
	require.NotNil(t, savedFinal.ApprovedBy)
	assert.Equal(t, "scheduler", *savedFinal.ApprovedBy, "the scheduler must stamp itself as approver")

	mockServiceClient.AssertExpectations(t)
}

// TestFireScheduledDelayedPurchases_AuditGapAfterCASStillFires covers the
// AUDIT GAP branch at scheduled_fire.go:97: the approved_by stamp write
// after a winning CAS is best-effort by design (a failed audit stamp must
// never block the purchase itself from firing). Asserts the purchase still
// fires and counts as Fired even when that intermediate save fails.
func TestFireScheduledDelayedPurchases_AuditGapAfterCASStillFires(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	mockEmail := new(MockEmailSender)
	mockFactory := new(MockProviderFactory)
	mockProviderInst := new(MockProvider)
	mockServiceClient := new(MockServiceClient)

	const acctID = "acct-auditgap"
	row := dueExec("exec-auditgap-fire")
	row.StepNumber = 1
	row.Recommendations = []config.RecommendationRecord{
		{
			Provider: "aws", Service: "ec2", ResourceType: "m5.large", Region: "us-east-1",
			Count: 1, UpfrontCost: 300, Savings: 60, Selected: true, CloudAccountID: aws.String(acctID),
		},
	}
	account := &config.CloudAccount{
		ID: acctID, Name: "Audit Gap Account", Provider: "aws", ExternalID: "444444444444", AWSAuthMode: "access_keys",
	}
	plan := &config.PurchasePlan{ID: "plan-1", Name: "Plan 1", RampSchedule: config.RampSchedule{TotalSteps: 1}}

	store.On("GetScheduledExecutionsDue", ctx).Return([]config.PurchaseExecution{row}, nil)

	approved := row
	approved.Status = "approved"
	store.On("TransitionExecutionStatus", ctx, "exec-auditgap-fire", []string{"scheduled"}, "running", (*string)(nil)).
		Return(&approved, nil)

	store.On("GetPurchasePlan", ctx, "plan-1").Return(plan, nil)
	store.GetPlanAccountsFn = func(_ context.Context, _ string) ([]config.CloudAccount, error) {
		return nil, nil
	}
	store.On("GetCloudAccount", ctx, acctID).Return(account, nil)
	store.On("CompletePlanStep", ctx, "plan-1", 1).Return(nil)

	// The approved_by audit-stamp save (the FIRST SavePurchaseExecution call,
	// right after the CAS) fails; the terminal save after executeAndFinalize
	// (the SECOND call) succeeds -- exactly the branch at scheduled_fire.go:97.
	var saveCalls int
	store.SavePurchaseExecutionFn = func(_ context.Context, e *config.PurchaseExecution) error {
		saveCalls++
		if saveCalls == 1 {
			return fmt.Errorf("connection reset by peer")
		}
		return nil
	}
	store.On("SavePurchaseHistory", ctx, mock.AnythingOfType("*config.PurchaseHistoryRecord")).Return(nil)
	mockEmail.On("SendPurchaseConfirmation", ctx, mock.AnythingOfType("email.NotificationData")).Return(nil)

	mockFactory.On("CreateAndValidateProvider", mock.Anything, "aws", mock.Anything).Return(mockProviderInst, nil)
	mockProviderInst.On("GetServiceClient", mock.Anything, common.ServiceEC2, mock.Anything).Return(mockServiceClient, nil)
	mockServiceClient.On("PurchaseCommitment", mock.Anything, mock.Anything, mock.Anything).
		Return(common.PurchaseResult{Success: true, CommitmentID: "ri-auditgap"}, nil).Once()

	mgr := &Manager{
		config:          store,
		email:           mockEmail,
		providerFactory: mockFactory,
		credStore:       awsAccessKeyCredStore(),
		dashboardURL:    "https://dashboard.example.com",
	}

	result, err := mgr.FireScheduledDelayedPurchases(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Fired,
		"a failed approved_by audit stamp must not block the purchase from firing (scheduled_fire.go:97)")
	assert.Equal(t, 0, result.RaceLost)
	assert.Equal(t, 0, result.Errored)
	assert.Equal(t, 2, saveCalls, "the audit-stamp save and the terminal save must both be attempted")

	mockServiceClient.AssertExpectations(t)
}

// TestFireScheduledDelayedPurchases_DelayPathNotSilentNoOp is a compile-time
// guard: if FireScheduledDelayedPurchases is removed from Manager or its
// signature drifts, the typed assertion below fails to build and catches the
// regression before the test suite runs.
func TestFireScheduledDelayedPurchases_DelayPathNotSilentNoOp(t *testing.T) {
	// Typed assertion ensures both method existence and signature are locked.
	// The explicit type is intentional: QF1011 notwithstanding, omitting it
	// would revert to the weaker method-existence-only guard that this
	// assertion replaced.
	var _ func(context.Context) (*FireResult, error) = (&Manager{}).FireScheduledDelayedPurchases //nolint:staticcheck // QF1011: explicit type is intentional to catch signature drift
}
