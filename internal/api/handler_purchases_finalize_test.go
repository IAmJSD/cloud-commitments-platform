package api

import (
	"context"
	"errors"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const finalizeExecID = "11111111-2222-4333-8444-555555555555"

func newFinalizeExecution() *config.PurchaseExecution {
	return &config.PurchaseExecution{ExecutionID: finalizeExecID, Status: "pending"}
}

// TestFinalizePurchaseStatus_GuardsFailedWriteOnPending pins issue #1232: the
// email-failure write must go through the status-guarded save with "pending"
// as the only expected status, never through the unguarded full-row save.
func TestFinalizePurchaseStatus_GuardsFailedWriteOnPending(t *testing.T) {
	store := new(MockConfigStore)
	store.On("SaveExecutionIfStatus", mock.Anything, mock.MatchedBy(func(e *config.PurchaseExecution) bool {
		return e.ExecutionID == finalizeExecID && e.Status == "failed" && e.Error == "send failed: throttled"
	}), []string{"pending"}).Return(nil).Once()
	h := &Handler{config: store}
	exec := newFinalizeExecution()

	status := h.finalizePurchaseStatus(context.Background(), exec, false, "send failed: throttled")

	assert.Equal(t, "failed", status)
	assert.Equal(t, "failed", exec.Status, "the caller's copy reflects the committed write")
	assert.Equal(t, "send failed: throttled", exec.Error)
	store.AssertExpectations(t)
	store.AssertNotCalled(t, "SavePurchaseExecution", mock.Anything, mock.Anything)
}

// TestFinalizePurchaseStatus_LostGuardIsNoOp is the approve-during-send case:
// another writer moved the row off "pending" while the email was in flight.
// The response must carry the row's real status, the caller's copy must stay
// untouched, and nothing may be written over the row.
func TestFinalizePurchaseStatus_LostGuardIsNoOp(t *testing.T) {
	for _, current := range []string{"approved", "running", "completed", "canceled", "paused"} {
		t.Run(current, func(t *testing.T) {
			store := new(MockConfigStore)
			store.On("SaveExecutionIfStatus", mock.Anything, mock.Anything, []string{"pending"}).
				Return(config.ErrExecutionNotInExpectedStatus).Once()
			store.On("GetExecutionByID", mock.Anything, finalizeExecID).
				Return(&config.PurchaseExecution{ExecutionID: finalizeExecID, Status: current}, nil).Once()
			h := &Handler{config: store}
			exec := newFinalizeExecution()

			status := h.finalizePurchaseStatus(context.Background(), exec, false, "send failed: throttled")

			assert.Equal(t, current, status)
			assert.Equal(t, "pending", exec.Status, "a lost guard must not mutate the caller's copy")
			assert.Empty(t, exec.Error)
			store.AssertExpectations(t)
			store.AssertNotCalled(t, "SavePurchaseExecution", mock.Anything, mock.Anything)
		})
	}
}

// TestFinalizePurchaseStatus_LostGuardReReadFails falls back to "pending" when
// the stored status cannot be re-read after a lost guard.
func TestFinalizePurchaseStatus_LostGuardReReadFails(t *testing.T) {
	store := new(MockConfigStore)
	store.On("SaveExecutionIfStatus", mock.Anything, mock.Anything, []string{"pending"}).
		Return(config.ErrExecutionNotInExpectedStatus).Once()
	store.On("GetExecutionByID", mock.Anything, finalizeExecID).Return(nil, errors.New("db down")).Once()
	h := &Handler{config: store}

	assert.Equal(t, "pending", h.finalizePurchaseStatus(context.Background(), newFinalizeExecution(), false, "x"))
	store.AssertExpectations(t)
}

// TestFinalizePurchaseStatus_WriteErrorKeepsPending keeps the pre-existing
// best-effort contract: any other save error is logged and reported as
// "pending" without a re-read.
func TestFinalizePurchaseStatus_WriteErrorKeepsPending(t *testing.T) {
	store := new(MockConfigStore)
	store.On("SaveExecutionIfStatus", mock.Anything, mock.Anything, []string{"pending"}).
		Return(errors.New("connection reset")).Once()
	h := &Handler{config: store}
	exec := newFinalizeExecution()

	assert.Equal(t, "pending", h.finalizePurchaseStatus(context.Background(), exec, false, "x"))
	assert.Equal(t, "pending", exec.Status)
	store.AssertExpectations(t)
	store.AssertNotCalled(t, "GetExecutionByID", mock.Anything, mock.Anything)
}

// TestFinalizePurchaseStatus_EmailSentWritesNothing: a sent email leaves the
// row pending and touches the store not at all.
func TestFinalizePurchaseStatus_EmailSentWritesNothing(t *testing.T) {
	store := new(MockConfigStore)
	h := &Handler{config: store}
	require.Equal(t, "pending", h.finalizePurchaseStatus(context.Background(), newFinalizeExecution(), true, ""))
	store.AssertNotCalled(t, "SaveExecutionIfStatus", mock.Anything, mock.Anything, mock.Anything)
	store.AssertNotCalled(t, "SavePurchaseExecution", mock.Anything, mock.Anything)
}
