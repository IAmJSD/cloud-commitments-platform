package api

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #260: the session-authed branch of the revoke endpoint
// (tryRevokeViaSession -> revokeViaSession) must enforce
// config.RevocationWindow just like the email-token branch does, so a
// cancel-any / cancel-own holder cannot flip an execution whose window closed
// long ago to revocation_requested.

const (
	sessWindowExecID = "26026026-0260-0260-0260-260260260260"
	sessWindowUserID = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	sessWindowEmail  = "admin@example.com"
	sessWindowToken  = "admin-window-token"
)

// newSessionWindowAuth returns an auth mock for a session admin holding
// cancel-any on purchases with unrestricted account scope and a passing CSRF
// check, so the request reaches revokeViaSession.
func newSessionWindowAuth(ctx context.Context) *MockAuthService {
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", ctx, sessWindowToken).
		Return(&Session{UserID: sessWindowUserID, Email: sessWindowEmail}, nil)
	mockAuth.On("HasPermissionAPI", ctx, sessWindowUserID, "cancel-any", "purchases").Return(true, nil)
	mockAuth.allowConstraintChecks()
	mockAuth.On("GetAllowedAccountsAPI", ctx, sessWindowUserID).Return([]string{}, nil)
	mockAuth.On("ValidateCSRFToken", ctx, sessWindowToken, "").Return(nil)
	return mockAuth
}

func sessionWindowRequest() *events.LambdaFunctionURLRequest {
	// No token: forces the tokenless session-auth branch.
	return &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer " + sessWindowToken},
	}
}

func TestRevokeViaEmailToken_SessionBranchRejectsClosedWindow(t *testing.T) {
	t.Parallel()
	longAgo := time.Now().Add(-30 * 24 * time.Hour)
	cases := map[string]func(*config.PurchaseExecution){
		"completed_at outside window": func(e *config.PurchaseExecution) { e.CompletedAt = &longAgo },
		"executed_at fallback outside window": func(e *config.PurchaseExecution) {
			e.ExecutedAt = &longAgo
		},
	}
	for name, stamp := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			exec := buildCompletedExec(sessWindowExecID, "unused-token")
			stamp(exec)

			mockStore := new(MockConfigStore)
			mockStore.On("GetExecutionByID", ctx, sessWindowExecID).Return(exec, nil)
			// Registered as Maybe so a regression (the write going through)
			// fails on the assertions below instead of panicking the mock.
			mockStore.On("TransitionExecutionStatus", mock.Anything, mock.Anything,
				mock.Anything, mock.Anything, mock.Anything).
				Return(&config.PurchaseExecution{ExecutionID: sessWindowExecID, Status: "revocation_requested"}, nil).Maybe()
			mockStore.On("SetCancelledBy", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
			mockAuth := newSessionWindowAuth(ctx)

			handler := &Handler{config: mockStore, auth: mockAuth}
			result, err := handler.revokeViaEmailToken(ctx, sessionWindowRequest(), sessWindowExecID, "")
			mockStore.AssertNotCalled(t, "TransitionExecutionStatus",
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			mockStore.AssertNotCalled(t, "SetCancelledBy", mock.Anything, mock.Anything, mock.Anything)
			require.Error(t, err, "session revoke after the window closed must be denied")
			assert.Nil(t, result)
			ce, ok := IsClientError(err)
			require.True(t, ok, "expected a ClientError, got %v", err)
			assert.Equal(t, 403, ce.code)
			assert.Contains(t, ce.message, "revocation window has closed")
			mockStore.AssertExpectations(t)
			mockAuth.AssertExpectations(t)
		})
	}
}

// TestRevokeViaEmailToken_SessionBranchAllowsOpenWindow guards against the
// window check over-blocking: a session revoke inside the window still records
// revocation_requested.
func TestRevokeViaEmailToken_SessionBranchAllowsOpenWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	recent := time.Now().Add(-2 * time.Hour)
	exec := buildCompletedExec(sessWindowExecID, "unused-token")
	exec.CompletedAt = &recent

	mockStore := new(MockConfigStore)
	mockStore.On("GetExecutionByID", ctx, sessWindowExecID).Return(exec, nil)
	mockStore.On("TransitionExecutionStatus", ctx, sessWindowExecID,
		[]string{"completed", "partially_completed"}, "revocation_requested",
		mock.MatchedBy(func(actor *string) bool { return actor != nil && *actor == sessWindowUserID })).
		Return(&config.PurchaseExecution{ExecutionID: sessWindowExecID, Status: "revocation_requested"}, nil)
	mockStore.On("SetCancelledBy", ctx, sessWindowExecID, sessWindowEmail).Return(nil)
	mockAuth := newSessionWindowAuth(ctx)

	handler := &Handler{config: mockStore, auth: mockAuth}
	result, err := handler.revokeViaEmailToken(ctx, sessionWindowRequest(), sessWindowExecID, "")
	require.NoError(t, err, "session revoke inside the window must succeed")
	resultMap, ok := result.(map[string]string)
	require.True(t, ok, "expected map result, got %T", result)
	assert.Equal(t, "revocation_requested", resultMap["status"])
	mockStore.AssertExpectations(t)
	mockAuth.AssertExpectations(t)
}
