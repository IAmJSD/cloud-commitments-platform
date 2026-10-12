package api

import (
	"context"
	"encoding/json"
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
	store.On("TransitionExecutionStatus", mock.Anything, tokenErrExecID, mock.Anything, "running", mock.Anything).
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
	store.On("CancelExecutionAtomic", mock.Anything, mock.Anything, tokenErrExecID, mock.Anything).
		Return(false, "approved", nil)

	resp, err := h.HandleRequest(context.Background(), tokenErrRequest("cancel", tokenErrRawToken))
	require.NoError(t, err)
	assert.Equal(t, 409, resp.StatusCode, resp.Body)
}
