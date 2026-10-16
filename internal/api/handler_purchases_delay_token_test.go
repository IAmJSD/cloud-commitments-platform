package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// The delayed-approval branch of the email-link approve must validate the
// token exactly like the immediate branch (issue #586), reusing the harness
// from handler_purchases_token_errors_test.go.

func delayTokenHandler(t *testing.T, exec *config.PurchaseExecution) *Handler {
	t.Helper()
	h := tokenErrHandler(t, exec, nil)
	store := h.config.(*MockConfigStore)
	kept := store.ExpectedCalls[:0]
	for _, c := range store.ExpectedCalls {
		if c.Method != "GetGlobalConfig" {
			kept = append(kept, c)
		}
	}
	store.ExpectedCalls = kept
	approver := "admin@example.com"
	store.On("GetGlobalConfig", mock.Anything).
		Return(&config.GlobalConfig{NotificationEmail: &approver, PurchaseDelayHours: 1}, nil)
	return h
}

func TestHandleRequest_ApproveViaToken_DelayConfigured_ValidatesToken(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	hash := config.HashApprovalToken(tokenErrRawToken)

	cases := []struct {
		name  string
		exec  config.PurchaseExecution
		token string
		want  int
	}{
		{"wrong token", config.PurchaseExecution{Status: "pending", ApprovalToken: hash, ApprovalTokenExpiresAt: &future}, "WRONG", 403},
		{"expired token", config.PurchaseExecution{Status: "pending", ApprovalToken: hash, ApprovalTokenExpiresAt: &past}, tokenErrRawToken, 410},
		{"execution without a stored token", config.PurchaseExecution{Status: "pending", ApprovalTokenExpiresAt: &future}, tokenErrRawToken, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec := tc.exec
			h := delayTokenHandler(t, &exec)
			store := h.config.(*MockConfigStore)
			// Registered so a regression answers 200 instead of panicking on an unmocked call.
			scheduled := &config.PurchaseExecution{ExecutionID: tokenErrExecID, Status: "scheduled"}
			store.On("TransitionExecutionStatus", mock.Anything, tokenErrExecID, mock.Anything, "scheduled", mock.Anything).
				Return(scheduled, nil).Maybe()
			store.On("TransitionExecutionStatusTokenPinned", mock.Anything, tokenErrExecID, mock.Anything, "scheduled", mock.Anything, mock.Anything).
				Return(scheduled, nil).Maybe()
			store.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(nil).Maybe()
			resp, err := h.HandleRequest(context.Background(), tokenErrRequest("approve", tc.token))
			require.NoError(t, err)
			assert.Equal(t, tc.want, resp.StatusCode, resp.Body)
			store.AssertNotCalled(t, "TransitionExecutionStatus",
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			store.AssertNotCalled(t, "TransitionExecutionStatusTokenPinned",
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

func TestHandleRequest_ApproveViaToken_DelayConfigured_ValidTokenSchedules(t *testing.T) {
	future := time.Now().Add(time.Hour)
	exec := &config.PurchaseExecution{Status: "pending", ApprovalToken: config.HashApprovalToken(tokenErrRawToken), ApprovalTokenExpiresAt: &future}
	h := delayTokenHandler(t, exec)
	store := h.config.(*MockConfigStore)
	scheduled := &config.PurchaseExecution{ExecutionID: tokenErrExecID, Status: "scheduled"}
	// The CAS must carry the supplied token so the store re-checks it against
	// the row at transition time, not only against the request's snapshot
	// (issue #590).
	store.On("TransitionExecutionStatusTokenPinned", mock.Anything, tokenErrExecID, []string{"pending", "notified"}, "scheduled", mock.Anything, tokenErrRawToken).
		Return(scheduled, nil).Once()
	store.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(nil)

	resp, err := h.HandleRequest(context.Background(), tokenErrRequest("approve", tokenErrRawToken))
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode, resp.Body)
	assert.Contains(t, resp.Body, "scheduled")
	store.AssertExpectations(t)
	store.AssertNotCalled(t, "TransitionExecutionStatus",
		mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestHandleRequest_ApproveViaToken_DelayConfigured_TokenRotatedBeforeCASIs403
// covers issue #590: the request's snapshot still holds the hash of the
// supplied token, so the pre-check passes, but a re-notification rotated the
// stored token before the status CAS. The token-pinned CAS rejects it, and the
// handler must answer 403 without scheduling or stamping the row.
func TestHandleRequest_ApproveViaToken_DelayConfigured_TokenRotatedBeforeCASIs403(t *testing.T) {
	future := time.Now().Add(time.Hour)
	exec := &config.PurchaseExecution{Status: "notified", ApprovalToken: config.HashApprovalToken(tokenErrRawToken), ApprovalTokenExpiresAt: &future}
	h := delayTokenHandler(t, exec)
	store := h.config.(*MockConfigStore)
	store.On("TransitionExecutionStatusTokenPinned", mock.Anything, tokenErrExecID, []string{"pending", "notified"}, "scheduled", mock.Anything, tokenErrRawToken).
		Return(nil, fmt.Errorf("%w: execution %s", config.ErrApprovalTokenChanged, tokenErrExecID)).Once()

	resp, err := h.HandleRequest(context.Background(), tokenErrRequest("approve", tokenErrRawToken))
	require.NoError(t, err)
	assert.Equal(t, 403, resp.StatusCode, resp.Body)
	assert.Contains(t, resp.Body, "invalid approval token")
	store.AssertExpectations(t)
	store.AssertNotCalled(t, "TransitionExecutionStatus",
		mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	store.AssertNotCalled(t, "SavePurchaseExecution", mock.Anything, mock.Anything)
}

func TestHandleRequest_ApproveViaToken_DelayConfigured_ReplayOfScheduledIs409(t *testing.T) {
	future := time.Now().Add(time.Hour)
	exec := &config.PurchaseExecution{Status: "scheduled", ApprovalToken: config.HashApprovalToken(tokenErrRawToken), ApprovalTokenExpiresAt: &future}
	h := delayTokenHandler(t, exec)
	store := h.config.(*MockConfigStore)
	store.On("TransitionExecutionStatusTokenPinned", mock.Anything, tokenErrExecID, []string{"pending", "notified"}, "scheduled", mock.Anything, tokenErrRawToken).
		Return(nil, config.ErrExecutionNotInExpectedStatus)

	resp, err := h.HandleRequest(context.Background(), tokenErrRequest("approve", tokenErrRawToken))
	require.NoError(t, err)
	assert.Equal(t, 409, resp.StatusCode, resp.Body)
}
