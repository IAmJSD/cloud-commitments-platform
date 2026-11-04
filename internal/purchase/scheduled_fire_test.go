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
	assertMocksOnCleanup(t, store, mockEmail, mockFactory, mockProviderInst)
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
	approved.ApprovedBy = aws.String("approver@example.com")
	store.On("TransitionExecutionStatus", ctx, "exec-scheduled-fire", []string{"scheduled"}, "running", (*string)(nil)).
		Return(&approved, nil)

	store.On("GetPurchasePlan", mock.Anything, "plan-1").Return(plan, nil)
	store.GetPlanAccountsFn = func(_ context.Context, _ string) ([]config.CloudAccount, error) {
		return nil, nil // no plan-level accounts -> single-account path
	}
	store.On("GetCloudAccount", mock.Anything, acctID).Return(account, nil)
	store.On("CompletePlanStep", mock.Anything, "plan-1", 1).Return(nil)

	var savedFinal *config.PurchaseExecution
	// The audit stamp and the terminal save are both guarded on running; an
	// unguarded SavePurchaseExecution would be an unexpected call here.
	store.On("SaveExecutionIfStatus", mock.Anything, mock.Anything, []string{"running"}).
		Run(func(args mock.Arguments) {
			c := *args.Get(1).(*config.PurchaseExecution)
			savedFinal = &c
		}).Return(nil)
	store.On("SavePurchaseHistory", mock.Anything, mock.AnythingOfType("*config.PurchaseHistoryRecord")).Return(nil)
	mockEmail.On("SendPurchaseConfirmation", mock.Anything, mock.AnythingOfType("email.NotificationData")).Return(nil)

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
	assert.Equal(t, "approver@example.com", *savedFinal.ApprovedBy, "the sweep must keep the stored approver")

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
	assertMocksOnCleanup(t, store, mockEmail, mockFactory, mockProviderInst)
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

	store.On("GetPurchasePlan", mock.Anything, "plan-1").Return(plan, nil)
	store.GetPlanAccountsFn = func(_ context.Context, _ string) ([]config.CloudAccount, error) {
		return nil, nil
	}
	store.On("GetCloudAccount", mock.Anything, acctID).Return(account, nil)
	store.On("CompletePlanStep", mock.Anything, "plan-1", 1).Return(nil)

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
	store.On("SavePurchaseHistory", mock.Anything, mock.AnythingOfType("*config.PurchaseHistoryRecord")).Return(nil)
	mockEmail.On("SendPurchaseConfirmation", mock.Anything, mock.AnythingOfType("email.NotificationData")).Return(nil)

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

// fireWithClaimedApprover fires one due row whose claimed copy carries the
// given ApprovedBy and returns the sweep result plus every guarded
// "running" save (the audit stamp and the terminal save).
func fireWithClaimedApprover(t *testing.T, approver *string) (FireResult, []config.PurchaseExecution) {
	t.Helper()
	ctx := context.Background()
	store := new(MockConfigStore)
	mockEmail := new(MockEmailSender)
	mockFactory := new(MockProviderFactory)
	mockProviderInst := new(MockProvider)
	assertMocksOnCleanup(t, store, mockEmail, mockFactory, mockProviderInst)
	mockServiceClient := new(MockServiceClient)

	const acctID = "acct-approver"
	row := dueExec("exec-approver-fire")
	row.StepNumber = 1
	row.Recommendations = []config.RecommendationRecord{
		{
			Provider: "aws", Service: "ec2", ResourceType: "m5.large", Region: "us-east-1",
			Count: 1, UpfrontCost: 300, Savings: 60, Selected: true, CloudAccountID: aws.String(acctID),
		},
	}
	account := &config.CloudAccount{
		ID: acctID, Name: "Approver Account", Provider: "aws", ExternalID: "555555555555", AWSAuthMode: "access_keys",
	}
	plan := &config.PurchasePlan{ID: "plan-1", Name: "Plan 1", RampSchedule: config.RampSchedule{TotalSteps: 1}}

	store.On("GetScheduledExecutionsDue", ctx).Return([]config.PurchaseExecution{row}, nil)
	claimed := row
	claimed.Status = "running"
	claimed.ApprovedBy = approver
	store.On("TransitionExecutionStatus", ctx, "exec-approver-fire", []string{"scheduled"}, "running", (*string)(nil)).
		Return(&claimed, nil)
	store.On("GetPurchasePlan", mock.Anything, "plan-1").Return(plan, nil)
	store.GetPlanAccountsFn = func(_ context.Context, _ string) ([]config.CloudAccount, error) {
		return nil, nil
	}
	store.On("GetCloudAccount", mock.Anything, acctID).Return(account, nil)
	store.On("CompletePlanStep", mock.Anything, "plan-1", 1).Return(nil)

	var saves []config.PurchaseExecution
	store.On("SaveExecutionIfStatus", mock.Anything, mock.Anything, []string{"running"}).
		Run(func(args mock.Arguments) {
			saves = append(saves, *args.Get(1).(*config.PurchaseExecution))
		}).Return(nil)
	store.On("SavePurchaseHistory", mock.Anything, mock.AnythingOfType("*config.PurchaseHistoryRecord")).Return(nil)
	mockEmail.On("SendPurchaseConfirmation", mock.Anything, mock.AnythingOfType("email.NotificationData")).Return(nil)
	mockFactory.On("CreateAndValidateProvider", mock.Anything, "aws", mock.Anything).Return(mockProviderInst, nil)
	mockProviderInst.On("GetServiceClient", mock.Anything, common.ServiceEC2, mock.Anything).Return(mockServiceClient, nil)
	mockServiceClient.On("PurchaseCommitment", mock.Anything, mock.Anything, mock.Anything).
		Return(common.PurchaseResult{Success: true, CommitmentID: "ri-approver"}, nil).Once()

	mgr := &Manager{
		config:          store,
		email:           mockEmail,
		providerFactory: mockFactory,
		credStore:       awsAccessKeyCredStore(),
		dashboardURL:    "https://dashboard.example.com",
	}
	result, err := mgr.FireScheduledDelayedPurchases(ctx)
	require.NoError(t, err)
	mockServiceClient.AssertExpectations(t)
	return *result, saves
}

// Issue #1589: the sweep used to overwrite the approver stored at approve
// time with the literal "scheduler".
func TestFireScheduledDelayedPurchases_KeepsApproverInApprovedBy(t *testing.T) {
	result, saves := fireWithClaimedApprover(t, aws.String("approver@example.com"))
	assert.Equal(t, 1, result.Fired)
	require.GreaterOrEqual(t, len(saves), 2, "audit stamp and terminal save are both guarded on running")
	for i, s := range saves {
		require.NotNil(t, s.ApprovedBy, "save %d lost the approver", i)
		assert.Equal(t, "approver@example.com", *s.ApprovedBy, "save %d", i)
	}
}

func TestFireScheduledDelayedPurchases_NilApproverStaysNil(t *testing.T) {
	result, saves := fireWithClaimedApprover(t, nil)
	assert.Equal(t, 1, result.Fired, "the purchase still runs without a stored approver")
	require.GreaterOrEqual(t, len(saves), 2)
	for i, s := range saves {
		assert.Nil(t, s.ApprovedBy, "save %d must not invent an approver", i)
	}
}

// fireFixture wires the same real provider chain as the EndToEnd test for n
// due rows, so a row that is not deferred really fires. hook runs inside each
// PurchaseCommitment call.
func fireFixture(t *testing.T, n int, hook func(call int)) (*Manager, *MockConfigStore, *MockServiceClient, []string) {
	t.Helper()
	store := new(MockConfigStore)
	mockEmail := new(MockEmailSender)
	mockFactory := new(MockProviderFactory)
	mockProviderInst := new(MockProvider)
	svc := new(MockServiceClient)

	const acctID = "acct-fire"
	account := &config.CloudAccount{
		ID: acctID, Name: "Fire Account", Provider: "aws", ExternalID: "333333333333", AWSAuthMode: "access_keys",
	}
	plan := &config.PurchasePlan{ID: "plan-1", Name: "Plan 1", RampSchedule: config.RampSchedule{TotalSteps: 1}}

	rows := make([]config.PurchaseExecution, 0, n)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("exec-budget-%d", i)
		row := dueExec(id)
		row.StepNumber = 1
		row.Recommendations = []config.RecommendationRecord{{
			Provider: "aws", Service: "ec2", ResourceType: "m5.large", Region: "us-east-1",
			Count: 1, UpfrontCost: 300, Savings: 60, Selected: true, CloudAccountID: aws.String(acctID),
		}}
		rows = append(rows, row)
		ids = append(ids, id)
		claimed := row
		claimed.Status = "running"
		store.On("TransitionExecutionStatus", mock.Anything, id, []string{"scheduled"}, "running", (*string)(nil)).
			Return(&claimed, nil).Maybe()
	}
	store.On("GetScheduledExecutionsDue", mock.Anything).Return(rows, nil)
	store.On("GetPurchasePlan", mock.Anything, "plan-1").Return(plan, nil).Maybe()
	store.GetPlanAccountsFn = func(_ context.Context, _ string) ([]config.CloudAccount, error) { return nil, nil }
	store.On("GetCloudAccount", mock.Anything, acctID).Return(account, nil).Maybe()
	store.On("CompletePlanStep", mock.Anything, "plan-1", 1).Return(nil).Maybe()
	store.On("SaveExecutionIfStatus", mock.Anything, mock.Anything, []string{"running"}).Return(nil).Maybe()
	store.On("SavePurchaseHistory", mock.Anything, mock.AnythingOfType("*config.PurchaseHistoryRecord")).Return(nil).Maybe()
	mockEmail.On("SendPurchaseConfirmation", mock.Anything, mock.AnythingOfType("email.NotificationData")).Return(nil).Maybe()
	mockFactory.On("CreateAndValidateProvider", mock.Anything, "aws", mock.Anything).Return(mockProviderInst, nil).Maybe()
	mockProviderInst.On("GetServiceClient", mock.Anything, common.ServiceEC2, mock.Anything).Return(svc, nil).Maybe()

	calls := 0
	svc.On("PurchaseCommitment", mock.Anything, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) {
			calls++
			if hook != nil {
				hook(calls)
			}
		}).
		Return(common.PurchaseResult{Success: true, CommitmentID: "ri-fired"}, nil).Maybe()

	mgr := &Manager{
		config: store, email: mockEmail, providerFactory: mockFactory,
		credStore: awsAccessKeyCredStore(), dashboardURL: "https://dashboard.example.com",
	}
	return mgr, store, svc, ids
}

// A sweep whose context has less than one purchase budget left must claim
// nothing: a claimed row would be cut off mid-purchase and strand in running
// (issue #1582).
func TestFireScheduledDelayedPurchases_DefersRowsWhenTimeBudgetShort(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mgr, store, svc, _ := fireFixture(t, 3, nil)

	result, err := mgr.FireScheduledDelayedPurchases(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, result.Found)
	assert.Equal(t, 0, result.Fired)
	assert.Equal(t, 3, result.Deferred)
	assert.Equal(t, 0, result.Errored)
	store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	svc.AssertNumberOfCalls(t, "PurchaseCommitment", 0)
}

// No deadline (CLI, tests) means enough time: the guard must not starve rows.
func TestFireScheduledDelayedPurchases_ProceedsWhenNoDeadline(t *testing.T) {
	mgr, _, svc, _ := fireFixture(t, 3, nil)

	result, err := mgr.FireScheduledDelayedPurchases(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 3, result.Fired)
	assert.Equal(t, 0, result.Deferred)
	svc.AssertNumberOfCalls(t, "PurchaseCommitment", 3)
}

// The first row starts with just over one budget left; while it runs the
// remaining time drops under the budget, so the second row is deferred
// unclaimed.
func TestFireScheduledDelayedPurchases_FirstRowFiresThenDefers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), purchaseRunBudget+500*time.Millisecond)
	defer cancel()
	mgr, store, svc, ids := fireFixture(t, 2, func(call int) {
		if call == 1 {
			time.Sleep(time.Second)
		}
	})

	result, err := mgr.FireScheduledDelayedPurchases(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Fired)
	assert.Equal(t, 1, result.Deferred)
	assert.Equal(t, 0, result.Errored)
	svc.AssertNumberOfCalls(t, "PurchaseCommitment", 1)
	store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, ids[1], []string{"scheduled"}, "running", (*string)(nil))
}

func TestNothingCouldFire(t *testing.T) {
	assert.True(t, nothingCouldFire(&FireResult{Found: 3, Deferred: 3}), "all rows deferred, none fired")
	assert.False(t, nothingCouldFire(&FireResult{Found: 3, Fired: 1, Deferred: 2}), "a fired row means the deadline works")
	assert.False(t, nothingCouldFire(&FireResult{Found: 3, Errored: 1, Deferred: 2}), "not every row was deferred")
	assert.False(t, nothingCouldFire(&FireResult{}), "no due rows")
}
