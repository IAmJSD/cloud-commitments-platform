package purchase

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func lostGuard() error {
	return fmt.Errorf("%w: execution x is %q", config.ErrExecutionNotInExpectedStatus, "failed")
}

func purchasedExec(status string) *config.PurchaseExecution {
	return &config.PurchaseExecution{
		ExecutionID: "exec-x", Status: status,
		Recommendations: []config.RecommendationRecord{{Provider: "aws", Service: "rds", Purchased: true}},
	}
}

// These assert on the guarded methods themselves: the mock falls back to
// SavePurchaseExecution when no guarded expectation is set, which would make a
// regression back to the unguarded save invisible.
func TestSaveOrRecoverTerminal_GuardsOnRunning(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	m := &Manager{config: store}
	exec := purchasedExec("completed")
	store.On("SaveExecutionIfStatus", ctx, exec, []string{"running"}).Return(nil).Once()

	require.NoError(t, m.saveOrRecoverTerminal(ctx, exec, nil))
	store.AssertCalled(t, "SaveExecutionIfStatus", ctx, exec, []string{"running"})
	store.AssertNotCalled(t, "SavePurchaseExecution", mock.Anything, mock.Anything)
	store.AssertNotCalled(t, "RecoverReapedExecution", mock.Anything, mock.Anything)
}

func TestSaveOrRecoverTerminal_ReapedButPurchasedIsRecovered(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	m := &Manager{config: store}
	exec := purchasedExec("partially_completed")
	store.On("SaveExecutionIfStatus", ctx, exec, []string{"running"}).Return(lostGuard())
	store.On("RecoverReapedExecution", ctx, exec).Return(nil)

	require.NoError(t, m.saveOrRecoverTerminal(ctx, exec, nil))
	assert.Contains(t, exec.Error, recoveredAfterReapNote)
	store.AssertCalled(t, "RecoverReapedExecution", ctx, exec)
}

func TestSaveOrRecoverTerminal_RetryLinkedFailsLoudWithDoubleBuyFlag(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	m := &Manager{config: store}
	exec := purchasedExec("completed")
	store.On("SaveExecutionIfStatus", ctx, exec, []string{"running"}).Return(lostGuard())
	store.On("RecoverReapedExecution", ctx, exec).Return(fmt.Errorf("%w: x", config.ErrExecutionRetryLinked))

	err := m.saveOrRecoverTerminal(ctx, exec, nil)
	require.ErrorIs(t, err, config.ErrExecutionRetryLinked)
	assert.Contains(t, err.Error(), "POSSIBLE DOUBLE BUY")
}

func TestSaveOrRecoverTerminal_NothingPurchasedIsNeverOverwritten(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	m := &Manager{config: store}
	exec := &config.PurchaseExecution{ExecutionID: "exec-x", Status: "failed"}
	store.On("SaveExecutionIfStatus", ctx, exec, []string{"running"}).Return(lostGuard())

	err := m.saveOrRecoverTerminal(ctx, exec, errors.New("boom"))
	require.ErrorIs(t, err, config.ErrExecutionNotInExpectedStatus)
	store.AssertNotCalled(t, "RecoverReapedExecution", mock.Anything, mock.Anything)
}

// The reaper's annotation is guarded on failed; losing it to a recovered row is
// benign and the row still counts as reaped.
func TestReapOne_AnnotationLostToRecoveryIsBenign(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	row := stuckExec("exec-R", "running")
	failed := row
	failed.Status = failedStatus
	store.On("TransitionExecutionStatus", ctx, "exec-R", stuckStatuses, failedStatus, (*string)(nil)).Return(&failed, nil)
	store.On("SaveExecutionIfStatus", ctx, mock.Anything, []string{failedStatus}).Return(lostGuard())

	result := &ReapResult{}
	newReaperManager(store).reapOne(ctx, &row, 10*time.Minute, time.Now(), result)
	assert.Equal(t, 1, result.Reaped)
	assert.Zero(t, result.Errored)
	store.AssertCalled(t, "SaveExecutionIfStatus", ctx, mock.Anything, []string{failedStatus})
}

// Approve claims straight to running and stamps the audit fields through the
// running-guarded save.
func TestApprove_ClaimsRunningAndStampsGuarded(t *testing.T) {
	ctx := context.Background()
	manager, store, sender := newApproveManager(t)
	updated := &config.PurchaseExecution{ExecutionID: "exec-g", PlanID: "plan-g", Status: "running", StepNumber: 1, Recommendations: approvalTestRecs()}
	store.On("GetExecutionByID", mock.Anything, "exec-g").Return(&config.PurchaseExecution{ExecutionID: "exec-g", PlanID: "plan-g", Status: "pending"}, nil).Maybe()
	store.On("TransitionExecutionStatus", ctx, "exec-g", approveFromStatuses, "running", (*string)(nil)).Return(updated, nil)
	stubExecuteChain(t, store, sender, "plan-g")
	store.On("SaveExecutionIfStatus", ctx, mock.Anything, []string{"running"}).Return(nil).Once()
	store.On("SaveExecutionIfStatus", mock.Anything, mock.Anything, []string{"running"}).Return(nil).Once()

	_, _, err := manager.ApproveAndExecute(ctx, "exec-g", "approver@example.invalid", nil)
	require.NoError(t, err)
	store.AssertCalled(t, "TransitionExecutionStatus", ctx, "exec-g", approveFromStatuses, "running", (*string)(nil))
	// Two guarded writes (audit stamp, terminal save); the only unguarded one is
	// the post-execution revocation-token rotation.
	store.AssertNumberOfCalls(t, "SaveExecutionIfStatus", 2)
	store.AssertNumberOfCalls(t, "SavePurchaseExecution", 1)
}
