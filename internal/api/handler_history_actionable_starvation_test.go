package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func executionsWithStatus(n int, status string, scheduled time.Time, idPrefix string) []config.PurchaseExecution {
	execs := make([]config.PurchaseExecution, n)
	for i := range execs {
		execs[i] = config.PurchaseExecution{
			ExecutionID:   fmt.Sprintf("%s-%d", idPrefix, i),
			Status:        status,
			ScheduledDate: scheduled,
		}
	}
	return execs
}

func isActionableCall(statuses []string) bool {
	return slices.Equal(statuses, actionableExecutionStatuses)
}

func isTerminalCall(statuses []string) bool {
	return slices.Equal(statuses, terminalExecutionStatuses)
}

func newStarvationStore(ctx context.Context) *MockConfigStore {
	store := new(MockConfigStore)
	store.On("GetAllPurchaseHistory", ctx, mock.Anything).Return([]config.PurchaseHistoryRecord{}, nil)
	store.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{}, nil).Maybe()
	return store
}

func runStarvationHistory(t *testing.T, ctx context.Context, store *MockConfigStore) map[string]any {
	t.Helper()
	mockAuth, req := adminHistoryReq(ctx)
	handler := &Handler{auth: mockAuth, config: store}
	result, err := handler.getHistory(ctx, req, map[string]string{})
	require.NoError(t, err)
	return historyJSON(t, result)
}

func TestHandler_getHistory_PendingNotStarvedByNewerTerminalRows(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	execs := executionsWithStatus(150, "failed", now, "failed")
	pending := executionsWithStatus(1, "pending", now.Add(-time.Hour), "pending")
	store := newStarvationStore(ctx)
	store.On("GetExecutionsByStatuses", ctx, mock.Anything, mock.Anything).Return(append(execs, pending...), nil)

	body := runStarvationHistory(t, ctx, store)

	ids := map[string]bool{}
	for _, p := range body["purchases"].([]any) {
		ids[p.(map[string]any)["purchase_id"].(string)] = true
	}
	assert.True(t, ids["pending-0"], "older pending row must survive 150 newer failed rows")
	summary := body["summary"].(map[string]any)
	assert.EqualValues(t, 1, summary["total_pending"])
}

func TestHandler_getHistory_ReturnsMoreThanDefaultLimitPendingRows(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	pending := executionsWithStatus(config.DefaultListLimit+50, "pending", now, "pending")
	store := newStarvationStore(ctx)
	store.On("GetExecutionsByStatuses", ctx, mock.Anything, mock.Anything).Return(pending, nil)

	body := runStarvationHistory(t, ctx, store)

	assert.Len(t, body["purchases"], config.DefaultListLimit+50)
	assert.EqualValues(t, config.DefaultListLimit+50, body["summary"].(map[string]any)["total_pending"])
	assert.Equal(t, false, body["truncated"])
}

func TestHandler_getHistory_FetchesDisjointClassesWithTheirLimits(t *testing.T) {
	ctx := context.Background()
	store := newStarvationStore(ctx)
	store.On("GetExecutionsByStatuses", ctx, mock.MatchedBy(isActionableCall), config.MaxListLimit).Return([]config.PurchaseExecution{}, nil).Once()
	store.On("GetExecutionsByStatuses", ctx, mock.MatchedBy(isTerminalCall), config.DefaultListLimit).Return([]config.PurchaseExecution{}, nil).Once()

	runStarvationHistory(t, ctx, store)

	store.AssertNumberOfCalls(t, "GetExecutionsByStatuses", 2)
	for _, s := range actionableExecutionStatuses {
		assert.NotContains(t, terminalExecutionStatuses, s, "classes must be disjoint")
	}
}

func TestHandler_getHistory_ClassListingErrorKeepsOtherClass(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name       string
		failActive bool
		wantID     string
	}{
		{"terminal error keeps actionable", false, "pending-0"},
		{"actionable error keeps terminal", true, "failed-0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := newStarvationStore(ctx)
			actionableRet := mock.Arguments{executionsWithStatus(1, "pending", now, "pending"), nil}
			terminalRet := mock.Arguments{executionsWithStatus(1, "failed", now, "failed"), nil}
			if tt.failActive {
				actionableRet = mock.Arguments{nil, errors.New("actionable listing failed")}
			} else {
				terminalRet = mock.Arguments{nil, errors.New("terminal listing failed")}
			}
			store.On("GetExecutionsByStatuses", ctx, mock.MatchedBy(isActionableCall), mock.Anything).Return(actionableRet...)
			store.On("GetExecutionsByStatuses", ctx, mock.MatchedBy(isTerminalCall), mock.Anything).Return(terminalRet...)

			body := runStarvationHistory(t, ctx, store)

			purchases := body["purchases"].([]any)
			require.Len(t, purchases, 1)
			assert.Equal(t, tt.wantID, purchases[0].(map[string]any)["purchase_id"])
		})
	}
}

func TestHandler_getHistory_TruncatedPerClass(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name          string
		actionable    int
		terminal      int
		wantTruncated bool
	}{
		{"terminal class at cap", 0, config.DefaultListLimit, true},
		{"both under cap", config.DefaultListLimit, config.DefaultListLimit - 1, false},
		{"actionable class at cap", config.MaxListLimit, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			rows := append(executionsWithStatus(tt.actionable, "pending", now, "pending"),
				executionsWithStatus(tt.terminal, "failed", now, "failed")...)
			store := newStarvationStore(ctx)
			store.On("GetExecutionsByStatuses", ctx, mock.Anything, mock.Anything).Return(rows, nil)

			body := runStarvationHistory(t, ctx, store)

			assert.Equal(t, tt.wantTruncated, body["truncated"])
		})
	}
}

func TestHandler_getHistory_StaleSweepBoundedToDefaultListLimit(t *testing.T) {
	t.Setenv("AWS_LAMBDA_RUNTIME_API", "127.0.0.1:9001")
	ctx := context.Background()
	stale := executionsWithStatus(150, "pending", time.Now().Add(-8*24*time.Hour), "stale")
	store := newStarvationStore(ctx)
	store.On("GetExecutionsByStatuses", ctx, mock.Anything, mock.Anything).Return(stale, nil)
	store.On("ExpireExecutionAtomic", mock.Anything, mock.Anything, mock.Anything).Return(false, nil)

	runStarvationHistory(t, ctx, store)

	store.AssertNumberOfCalls(t, "ExpireExecutionAtomic", config.DefaultListLimit)
}

// TestHandler_getHistory_DueApprovalNotCrowdedOutByFutureRampSteps is the
// issue #680 regression through the real getHistory path: 20 plans x 52
// future pending ramp steps exceed the actionable cap, and the pending
// approval due today must still be returned, with truncated set.
func TestHandler_getHistory_DueApprovalNotCrowdedOutByFutureRampSteps(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	var execs []config.PurchaseExecution
	for p := 0; p < 20; p++ {
		for s := 1; s <= 52; s++ {
			execs = append(execs, config.PurchaseExecution{
				ExecutionID:   fmt.Sprintf("ramp-%d-%d", p, s),
				Status:        "pending",
				ScheduledDate: now.AddDate(0, 0, 7*s),
			})
		}
	}
	execs = append(execs, executionsWithStatus(1, "pending", now.Add(-time.Hour), "due")...)
	store := newStarvationStore(ctx)
	store.On("GetExecutionsByStatuses", ctx, mock.Anything, mock.Anything).Return(execs, nil)

	body := runStarvationHistory(t, ctx, store)

	ids := map[string]bool{}
	for _, p := range body["purchases"].([]any) {
		ids[p.(map[string]any)["purchase_id"].(string)] = true
	}
	assert.True(t, ids["due-0"], "pending approval due today must survive 1040 future ramp steps")
	assert.True(t, ids["ramp-0-1"], "the soonest future ramp step survives the cap")
	assert.False(t, ids["ramp-0-52"], "the farthest future ramp steps are the ones dropped")
	assert.Len(t, body["purchases"], config.MaxListLimit)
	assert.Equal(t, true, body["truncated"])
}
