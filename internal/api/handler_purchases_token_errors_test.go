package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
)

// Email-link approve/cancel and the email revoke endpoint are driven through
// HandleRequest with the real purchase.Manager so the real token and status
// errors reach the HTTP mapping (issue #564).

const tokenErrExecID = "12345678-1234-1234-1234-123456789abc"
const tokenErrRawToken = "raw-secret-token"

func tokenErrHandler(t *testing.T, exec *config.PurchaseExecution, getErr error) *Handler {
	t.Helper()
	approver := "admin@example.com"
	accountID := "acct-1"
	store := new(MockConfigStore)
	store.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
		return &config.CloudAccount{ID: id, ContactEmail: approver}, nil
	}
	if getErr != nil {
		store.On("GetExecutionByID", mock.Anything, tokenErrExecID).Return(nil, getErr)
	} else {
		exec.ExecutionID = tokenErrExecID
		exec.Recommendations = []config.RecommendationRecord{{ID: "r1", CloudAccountID: &accountID}}
		store.On("GetExecutionByID", mock.Anything, tokenErrExecID).Return(exec, nil)
	}
	store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{NotificationEmail: &approver}, nil)

	auth := new(MockAuthService)
	auth.On("GetAllowedAccountsAPI", mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	auth.On("ValidateSession", mock.Anything, "sess-tok").Return(&Session{Email: approver}, nil)
	// CSRF is validated before the execution lookup (issue #597).
	auth.On("ValidateCSRFToken", mock.Anything, "sess-tok", "").Return(nil).Maybe()
	for _, perm := range []string{"approve-any", "approve-own", "cancel-any", "cancel-own", "revoke-any", "revoke-own"} {
		auth.On("HasPermissionAPI", mock.Anything, "", perm, "purchases").Return(false, nil).Maybe()
	}

	return &Handler{
		config:   store,
		auth:     auth,
		purchase: purchase.NewManager(purchase.ManagerConfig{ConfigStore: store}),
	}
}

func tokenErrRequest(action, token string) *events.LambdaFunctionURLRequest {
	return &events.LambdaFunctionURLRequest{
		QueryStringParameters: map[string]string{"token": token},
		Headers:               map[string]string{"authorization": "Bearer sess-tok"},
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{
				Method: "POST",
				Path:   "/api/purchases/" + action + "/" + tokenErrExecID,
			},
		},
	}
}

func TestHandleRequest_TokenActions_TokenAndStatusErrorsAre4xx(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	hash := config.HashApprovalToken(tokenErrRawToken)

	cases := []struct {
		name   string
		action string
		exec   config.PurchaseExecution
		token  string
		want   int
	}{
		{"approve invalid token", "approve", config.PurchaseExecution{Status: "pending", ApprovalToken: hash, ApprovalTokenExpiresAt: &future}, "wrong", 403},
		{"approve expired token", "approve", config.PurchaseExecution{Status: "pending", ApprovalToken: hash, ApprovalTokenExpiresAt: &past}, tokenErrRawToken, 410},
		{"cancel invalid token", "cancel", config.PurchaseExecution{Status: "pending", ApprovalToken: hash, ApprovalTokenExpiresAt: &future}, "wrong", 403},
		{"cancel expired token", "cancel", config.PurchaseExecution{Status: "pending", ApprovalToken: hash, ApprovalTokenExpiresAt: &past}, tokenErrRawToken, 410},
		{"cancel non-cancelable status", "cancel", config.PurchaseExecution{Status: "completed", ApprovalToken: hash, ApprovalTokenExpiresAt: &future}, tokenErrRawToken, 409},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec := tc.exec
			h := tokenErrHandler(t, &exec, nil)
			resp, err := h.HandleRequest(context.Background(), tokenErrRequest(tc.action, tc.token))
			require.NoError(t, err)
			assert.Equal(t, tc.want, resp.StatusCode, resp.Body)
			var body map[string]any
			require.NoError(t, json.Unmarshal([]byte(resp.Body), &body))
			assert.NotEmpty(t, body["error"])
		})
	}
}

func TestHandleRequest_ApproveViaToken_NonPendingExecutionIs409(t *testing.T) {
	future := time.Now().Add(time.Hour)
	exec := &config.PurchaseExecution{Status: "completed", ApprovalToken: config.HashApprovalToken(tokenErrRawToken), ApprovalTokenExpiresAt: &future}
	h := tokenErrHandler(t, exec, nil)
	store := h.config.(*MockConfigStore)
	store.On("TransitionExecutionStatusTokenPinned", mock.Anything, tokenErrExecID, mock.Anything, "running", mock.Anything, tokenErrRawToken).
		Return(nil, config.ErrExecutionNotInExpectedStatus)

	resp, err := h.HandleRequest(context.Background(), tokenErrRequest("approve", tokenErrRawToken))
	require.NoError(t, err)
	assert.Equal(t, 409, resp.StatusCode, resp.Body)
}

func TestHandleRequest_RevokeViaEmail_MissingExecutionIs404(t *testing.T) {
	h := tokenErrHandler(t, nil, config.ErrNotFound)
	resp, err := h.HandleRequest(context.Background(), tokenErrRequest("revoke", tokenErrRawToken))
	require.NoError(t, err)
	assert.Equal(t, 404, resp.StatusCode, resp.Body)
}

func TestHandleRequest_CancelViaToken_LostCASIs409(t *testing.T) {
	future := time.Now().Add(time.Hour)
	exec := &config.PurchaseExecution{Status: "pending", ApprovalToken: config.HashApprovalToken(tokenErrRawToken), ApprovalTokenExpiresAt: &future}
	h := tokenErrHandler(t, exec, nil)
	store := h.config.(*MockConfigStore)
	store.On("CancelExecutionAtomicTokenPinned", mock.Anything, mock.Anything, tokenErrExecID, mock.Anything, tokenErrRawToken).
		Return(false, "approved", nil)

	resp, err := h.HandleRequest(context.Background(), tokenErrRequest("cancel", tokenErrRawToken))
	require.NoError(t, err)
	assert.Equal(t, 409, resp.StatusCode, resp.Body)
}

// Issue #1190: the email-link approve and cancel pin the approval token inside
// the status CAS. A token rotated (re-notification) between the snapshot check
// and the CAS loses the pin with config.ErrApprovalTokenChanged, which is the
// caller's fault (their link is no longer the current one), so it answers 403
// "invalid approval token" like a wrong token, not a 500.
func TestHandleRequest_TokenActions_TokenRotatedBeforeCASIs403(t *testing.T) {
	future := time.Now().Add(time.Hour)
	rotated := fmt.Errorf("approve: %w: execution %s", config.ErrApprovalTokenChanged, tokenErrExecID)
	for _, action := range []string{"approve", "cancel"} {
		t.Run(action, func(t *testing.T) {
			exec := &config.PurchaseExecution{Status: "notified", ApprovalToken: config.HashApprovalToken(tokenErrRawToken), ApprovalTokenExpiresAt: &future}
			h := tokenErrHandler(t, exec, nil)
			store := h.config.(*MockConfigStore)
			store.On("TransitionExecutionStatusTokenPinned", mock.Anything, tokenErrExecID, []string{"pending", "notified"}, "running", (*string)(nil), tokenErrRawToken).
				Return(nil, rotated).Maybe()
			store.On("CancelExecutionAtomicTokenPinned", mock.Anything, mock.Anything, tokenErrExecID, mock.Anything, tokenErrRawToken).
				Return(false, "notified", rotated).Maybe()

			resp, err := h.HandleRequest(context.Background(), tokenErrRequest(action, tokenErrRawToken))
			require.NoError(t, err)
			assert.Equal(t, 403, resp.StatusCode, resp.Body)
			assert.Contains(t, resp.Body, "invalid approval token")
			if action == "approve" {
				store.AssertCalled(t, "TransitionExecutionStatusTokenPinned", mock.Anything, tokenErrExecID, []string{"pending", "notified"}, "running", (*string)(nil), tokenErrRawToken)
			} else {
				store.AssertCalled(t, "CancelExecutionAtomicTokenPinned", mock.Anything, mock.Anything, tokenErrExecID, mock.Anything, tokenErrRawToken)
			}
			store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			store.AssertNotCalled(t, "CancelExecutionAtomic", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

// Issue #1190: the revoke endpoint compares the token before the link's expiry
// and the revocation window, as approve does, so a wrong token gets the same
// 403 "invalid revocation token" whether the row's link is live, expired or
// past its window; only the right token learns that the link has lapsed.
func TestHandleRequest_RevokeViaEmail_WrongTokenOnLapsedRowIsInvalid(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	longAgo := time.Now().Add(-2 * config.RevocationWindow)
	hash := config.HashApprovalToken(tokenErrRawToken)
	cases := []struct {
		name      string
		exec      config.PurchaseExecution
		token     string
		want      int
		wantInMsg string
	}{
		{"wrong token on an expired link", config.PurchaseExecution{Status: "completed", ApprovalToken: hash, ApprovalTokenExpiresAt: &past}, "wrong", 403, "invalid revocation token"},
		{"wrong token past the revocation window", config.PurchaseExecution{Status: "completed", ApprovalToken: hash, ApprovalTokenExpiresAt: &future, CompletedAt: &longAgo}, "wrong", 403, "invalid revocation token"},
		{"right token on an expired link", config.PurchaseExecution{Status: "completed", ApprovalToken: hash, ApprovalTokenExpiresAt: &past}, tokenErrRawToken, 409, "expired"},
		{"right token past the revocation window", config.PurchaseExecution{Status: "completed", ApprovalToken: hash, ApprovalTokenExpiresAt: &future, CompletedAt: &longAgo}, tokenErrRawToken, 403, "revocation window has closed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec := tc.exec
			h := tokenErrHandler(t, &exec, nil)
			resp, err := h.HandleRequest(context.Background(), tokenErrRequest("revoke", tc.token))
			require.NoError(t, err)
			assert.Equal(t, tc.want, resp.StatusCode, resp.Body)
			assert.Contains(t, resp.Body, tc.wantInMsg)
			if tc.token != tokenErrRawToken {
				assert.NotContains(t, resp.Body, "expired")
				assert.NotContains(t, resp.Body, "window")
			}
		})
	}
}
