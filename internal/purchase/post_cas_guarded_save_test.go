package purchase

// Issue #835: the post-CAS saves outside the executors are status-guarded, and
// a lost claim found by the post-claim audit stamp stops the run before any
// provider call. Mock store and mock provider (labeled fixtures; nothing here
// reaches a cloud API). The Postgres replays of the races are in
// post_cas_guarded_save_integration_test.go.
//
// These assert on the guarded store methods themselves: the mock falls back to
// SavePurchaseExecution(Tx) when no guarded expectation is set, so a
// regression back to the unguarded save would otherwise pass unnoticed.

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

func TestSafeFail_StampIsGuardedOnFailed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		saveErr error
	}{
		{"guard holds", nil},
		// The flip to failed already landed; a lost stamp is an audit gap, not
		// a sweep error, and the row still counts as recovered.
		{"guard lost", lostGuard()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := new(MockConfigStore)
			stale := &config.PurchaseExecution{ExecutionID: "exec-sf", Status: "approved"}
			failed := &config.PurchaseExecution{ExecutionID: "exec-sf", Status: config.StatusFailed}
			store.On("TransitionExecutionStatus", ctx, "exec-sf", []string{"approved"}, config.StatusFailed, (*string)(nil)).Return(failed, nil)
			store.On("SaveExecutionIfStatus", ctx, failed, []string{config.StatusFailed}).Return(tc.saveErr).Once()

			counted, err := (&Manager{config: store}).safeFail(ctx, stale)

			require.NoError(t, err)
			assert.True(t, counted)
			assert.Contains(t, failed.Error, "failed by the recovery sweep")
			store.AssertExpectations(t)
			store.AssertNotCalled(t, "SavePurchaseExecution", mock.Anything, mock.Anything)
		})
	}
}

func TestFailExistingPlanStep_SaveIsGuardedOnFailed(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	row := &config.PurchaseExecution{ExecutionID: "exec-fps", PlanID: "plan-fps", StepNumber: 2, Status: "notified"}
	failed := *row
	failed.Status = config.StatusFailed
	store.On("TransitionExecutionStatusPinned", ctx, "exec-fps", []string{"pending", "notified"}, config.StatusFailed, (*string)(nil), true).Return(&failed, nil)
	store.On("SaveExecutionIfStatusTx", ctx, mock.Anything, &failed, []string{config.StatusFailed}).Return(nil).Once()
	store.On("DeleteSuppressionsByExecutionTx", ctx, mock.Anything, "exec-fps").Return(nil).Once()

	err := (&Manager{config: store}).failExistingPlanStep(ctx, row, "no recommendations")

	require.ErrorIs(t, err, errExecutionNotNotifiable, "a recorded failure is still the tick's quiet skip")
	assert.True(t, failed.NoSubmission)
	store.AssertExpectations(t)
	store.AssertNotCalled(t, "SavePurchaseExecutionTx", mock.Anything, mock.Anything, mock.Anything)
}

// A lost guard writes nothing: the suppression release shares the transaction
// and is never reached.
func TestFailExistingPlanStep_LostGuardReleasesNothing(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	row := &config.PurchaseExecution{ExecutionID: "exec-fps", PlanID: "plan-fps", StepNumber: 2, Status: "pending"}
	failed := *row
	failed.Status = config.StatusFailed
	store.On("TransitionExecutionStatusPinned", ctx, "exec-fps", []string{"pending", "notified"}, config.StatusFailed, (*string)(nil), true).Return(&failed, nil)
	store.On("SaveExecutionIfStatusTx", ctx, mock.Anything, &failed, []string{config.StatusFailed}).Return(lostGuard()).Once()

	err := (&Manager{config: store}).failExistingPlanStep(ctx, row, "no recommendations")

	require.ErrorIs(t, err, config.ErrExecutionNotInExpectedStatus)
	store.AssertExpectations(t)
	store.AssertNotCalled(t, "DeleteSuppressionsByExecutionTx", mock.Anything, mock.Anything, mock.Anything)
}

func TestRecordRampAdvanceRefusal_SaveIsGuardedOnTerminalStatus(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	exec := &config.PurchaseExecution{ExecutionID: "exec-rr", PlanID: "plan-rr", StepNumber: 1, Status: config.StatusCompleted}
	store.On("SaveExecutionIfStatus", ctx, exec, []string{config.StatusCompleted}).Return(lostGuard()).Once()

	(&Manager{config: store}).recordRampAdvanceRefusal(ctx, exec, errors.New("plan row changed"))

	assert.Contains(t, exec.Error, "ramp not advanced")
	store.AssertExpectations(t)
	store.AssertNotCalled(t, "SavePurchaseExecution", mock.Anything, mock.Anything)
}

// stampLostManager is an approve manager whose post-claim audit stamp loses
// its guard: another writer moved the claimed row on before the stamp.
func stampLostManager(t *testing.T, factory *MockProviderFactory, stampErr error) (*Manager, *MockConfigStore) {
	t.Helper()
	store := new(MockConfigStore)
	failOnProviderCall(factory)
	claimed := &config.PurchaseExecution{ExecutionID: "exec-st", PlanID: "plan-st", Status: "running", StepNumber: 1, ScheduledDate: time.Now(), Recommendations: approvalTestRecs()}
	store.On("GetExecutionByID", mock.Anything, "exec-st").Return(&config.PurchaseExecution{ExecutionID: "exec-st", PlanID: "plan-st", Status: "notified"}, nil).Maybe()
	store.On("TransitionExecutionStatus", mock.Anything, "exec-st", mock.Anything, "running", (*string)(nil)).Return(claimed, nil).Once()
	store.On("SaveExecutionIfStatus", mock.Anything, claimed, executorOwnedStatuses).Return(stampErr).Once()
	return &Manager{config: store, providerFactory: factory}, store
}

// failOnProviderCall makes any provider construction fail AND be recorded, so
// AssertNotCalled can see an attempt.
func failOnProviderCall(factory *MockProviderFactory) {
	factory.On("CreateAndValidateProvider", mock.Anything, mock.Anything, mock.Anything).
		Return(nil, errors.New("the provider must not be reached")).Maybe()
}

func TestApprove_StampFindsClaimLostBuysNothing(t *testing.T) {
	ctx := context.Background()
	factory := new(MockProviderFactory)
	m, store := stampLostManager(t, factory, lostGuard())

	row, _, err := m.ApproveAndExecute(ctx, "exec-st", "approver@example.invalid", nil, nil)

	require.ErrorIs(t, err, config.ErrExecutionNotInExpectedStatus, "callers map a lost claim to a 409")
	require.ErrorIs(t, err, errClaimLostBeforePurchase)
	assert.Nil(t, row, "no row is returned for a run that never reached the provider")
	factory.AssertNotCalled(t, "CreateAndValidateProvider", mock.Anything, mock.Anything, mock.Anything)
	store.AssertExpectations(t)
	store.AssertNumberOfCalls(t, "SaveExecutionIfStatus", 1)
}

func TestFireOneDue_StampFindsClaimLostBuysNothing(t *testing.T) {
	ctx := context.Background()
	factory := new(MockProviderFactory)
	m, store := stampLostManager(t, factory, lostGuard())

	outcome := m.fireOneDue(ctx, &config.PurchaseExecution{ExecutionID: "exec-st", Status: "scheduled"})

	assert.Equal(t, fireRaceLost, outcome, "a claim lost before the purchase is a race loss, not an error")
	factory.AssertNotCalled(t, "CreateAndValidateProvider", mock.Anything, mock.Anything, mock.Anything)
	store.AssertExpectations(t)
	store.AssertNumberOfCalls(t, "SaveExecutionIfStatus", 1)
}

// A store error that is not a lost guard leaves the claim this run's, so the
// stamp stays best-effort and the purchase goes ahead.
func TestStampClaimedAudit_StoreErrorStaysBestEffort(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	exec := &config.PurchaseExecution{ExecutionID: "exec-be", Status: "running"}
	store.On("SaveExecutionIfStatus", ctx, exec, executorOwnedStatuses).Return(errors.New("connection reset")).Once()

	require.NoError(t, (&Manager{config: store}).stampClaimedAudit(ctx, exec, "approved_by"))
	store.AssertExpectations(t)
}

func TestStampClaimedAudit_MissingRowIsClaimLost(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	exec := &config.PurchaseExecution{ExecutionID: "exec-gone", Status: "running"}
	store.On("SaveExecutionIfStatus", ctx, exec, executorOwnedStatuses).Return(config.ErrNotFound).Once()

	err := (&Manager{config: store}).stampClaimedAudit(ctx, exec, "approved_by")

	require.ErrorIs(t, err, errClaimLostBeforePurchase)
	require.ErrorIs(t, err, config.ErrNotFound)
	store.AssertExpectations(t)
}
