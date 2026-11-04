package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/email"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// recordingExecutedNotifier captures the NotificationData passed to
// SendPurchaseExecutedNotification so the execution-flow tests can assert the
// post-execution notification (issue #291) fires with the expected recipients
// and body fingerprints. All other SenderInterface methods are no-ops via the
// embedded stubEmailNotifier.
type recordingExecutedNotifier struct {
	stubEmailNotifier
	calls    int
	captured email.NotificationData
}

func (r *recordingExecutedNotifier) SendPurchaseExecutedNotification(_ context.Context, data email.NotificationData) error {
	r.calls++
	r.captured = data
	return nil
}

// assertExecutedNotificationFingerprints asserts the shared invariants of the
// post-execution notification across all three execution paths: it fired
// exactly once, resolved the per-account contact email as the primary To, and
// carried the expected revocation token + the executor in the body.
//
// expectedToken is the raw revocation token purchase.Manager.ApproveAndExecute
// mints and returns for this specific approval (issue #103): all three paths
// (token-authed approve, session approve, direct-execute) fund it from the
// mock's return value now, since only the token hash is stored and a
// re-read of the execution can never again yield a raw, emailable token.
func assertExecutedNotificationFingerprints(t *testing.T, n *recordingExecutedNotifier, contact, executedBy, expectedToken string) {
	t.Helper()
	require.Equal(t, 1, n.calls, "SendPurchaseExecutedNotification must fire exactly once")
	assert.Equal(t, contact, n.captured.RecipientEmail,
		"primary To must be the per-account contact email")
	assert.Equal(t, executedBy, n.captured.ExecutedBy,
		"body must record the executing actor")
	assert.Equal(t, expectedToken, n.captured.RevocationToken,
		"email must carry the expected revocation token")
	assert.NotEmpty(t, n.captured.ExecutedAt, "executed-at timestamp must be set")
}

// TestExecutedNotification_TokenApprovePath covers the email one-click
// (token-authed) approve branch of approvePurchase: after ApproveExecution
// succeeds, sendPurchaseExecutedEmail must fire with the fresh revocation
// token ApproveExecution returns, not the stale pre-approve token.
//
// This is the regression test for the defect where approveViaToken passed
// the stale pre-approve execution struct's ApprovalToken to
// sendPurchaseExecutedEmail. mintRevocationToken (called inside
// ApproveExecution's ApproveAndExecute) had already overwritten
// ApprovalToken in the DB, so the email embedded the old consumed token which
// validateRevokeToken rejected with 403 on every revoke attempt.
//
// Post issue #103 (only the token hash stored), the fix can no longer be
// "re-fetch the row" -- a re-read only ever yields the hash. The token must
// come directly from ApproveExecution's return value, which this test pins.
func TestExecutedNotification_TokenApprovePath(t *testing.T) {
	ctx := context.Background()
	execID := "12345678-1234-1234-1234-123456789abc"
	contact := "contact@example.com"
	freshToken := "fresh-revoke-token"
	accountID := "acct-1"
	recentCompleted := time.Now().Add(-1 * time.Minute)

	// pre-approve: pending execution with the original approval token.
	mockConfig := new(MockConfigStore)
	exec := approvalTestExec(execID, contact, mockConfig)

	// post-approve: completed execution as re-fetched for the OTHER email
	// fields (status, completed_at). Its ApprovalToken is the SHA-256 hash
	// mintRevocationToken persisted for freshToken (issue #103) -- never the
	// raw value, which only the ApproveExecution return carries.
	freshExec := &config.PurchaseExecution{
		ExecutionID:   execID,
		ApprovalToken: config.HashApprovalToken(freshToken),
		Status:        "completed",
		CompletedAt:   &recentCompleted,
		Recommendations: []config.RecommendationRecord{
			{ID: "r1", CloudAccountID: &accountID},
		},
	}

	// First call: getExecutionOr404 fetches the pending execution.
	// Second call: approveViaToken re-fetches after ApproveExecution returns,
	// to pick up the FINAL execution state (status, completed_at) for the
	// email body -- not the token, which comes from the return value below.
	mockConfig.On("GetExecutionByID", ctx, execID).Return(exec, nil).Once()
	mockConfig.On("GetExecutionByID", ctx, execID).Return(freshExec, nil).Once()
	mockConfig.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{
		NotificationEmail: &contact,
	}, nil)

	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	mockAuth.On("GetAllowedAccountsAPI", mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	mockAuth.On("ValidateSession", ctx, "sess-tok").Return(&Session{Email: contact}, nil)
	// CSRF is validated before the execution lookup (issue #597).
	mockAuth.On("ValidateCSRFToken", ctx, "sess-tok", "").Return(nil)
	mockAuth.On("HasPermissionAPI", ctx, "", "approve-any", "purchases").Return(false, nil).Maybe()
	mockAuth.On("HasPermissionAPI", ctx, "", "approve-own", "purchases").Return(false, nil).Maybe()

	mockPurchase := new(MockPurchaseManager)
	mockPurchase.On("ApproveExecution", ctx, execID, "valid-token", contact).Return(nil, freshToken, nil)

	notifier := &recordingExecutedNotifier{}
	handler := &Handler{
		purchase:      mockPurchase,
		config:        mockConfig,
		auth:          mockAuth,
		emailNotifier: notifier,
	}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer sess-tok"},
	}
	result, err := handler.approvePurchase(ctx, req, execID, "valid-token")
	require.NoError(t, err)
	assert.Equal(t, "completed", result.(map[string]string)["status"])

	// The email must carry the fresh revocation token ApproveExecution
	// returned, not the stale "valid-token" from the pre-approve struct.
	assertExecutedNotificationFingerprints(t, notifier, contact, contact, freshToken)

	// Confirm the fresh token is actually valid for revocation: validateRevokeToken
	// against the post-approve execution (whose ApprovalToken is the token's
	// hash, matching what mintRevocationToken actually persists) must succeed.
	// This guards the end-to-end scenario: recipient clicks "Revoke" in the
	// email -> token validates -> 200.
	require.NoError(t, validateRevokeToken(freshExec, freshToken),
		"the token embedded in the email must pass validateRevokeToken on the post-approve execution")

	mockPurchase.AssertExpectations(t)
	mockConfig.AssertExpectations(t)
}

// TestExecutedNotification_TokenApprovePath_RefetchFailureStillEmailsToken is
// the regression test for the degraded-path case: when the post-approve
// re-fetch (for the email's OTHER fields -- status, completed_at) fails,
// approveViaToken must still email a VALID revocation token, because that
// token comes from ApproveExecution's return value, not from the re-fetch
// (issue #103: a re-read of the execution can only ever yield the hash, so
// the old "re-fetch to get the fresh token, blank it on failure" design is no
// longer possible -- and no longer necessary, since the return value is
// already in hand regardless of the re-fetch outcome). The email falls back
// to the pre-approve execution snapshot for its other, cosmetic fields only.
func TestExecutedNotification_TokenApprovePath_RefetchFailureStillEmailsToken(t *testing.T) {
	ctx := context.Background()
	execID := "12345678-1234-1234-1234-123456789abc"
	contact := "contact@example.com"
	freshToken := "fresh-revoke-token-2"

	mockConfig := new(MockConfigStore)
	exec := approvalTestExec(execID, contact, mockConfig)

	// First call: getExecutionOr404 fetches the pending execution.
	// Second call: approveViaToken re-fetches for the final email state,
	// but this time the store errors -- the handler must fall back to the
	// pre-approve snapshot for those fields, and still emails the token.
	mockConfig.On("GetExecutionByID", ctx, execID).Return(exec, nil).Once()
	mockConfig.On("GetExecutionByID", ctx, execID).
		Return(nil, errors.New("transient store failure")).Once()
	mockConfig.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{
		NotificationEmail: &contact,
	}, nil)

	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	mockAuth.On("GetAllowedAccountsAPI", mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	mockAuth.On("ValidateSession", ctx, "sess-tok").Return(&Session{Email: contact}, nil)
	// CSRF is validated before the execution lookup (issue #597).
	mockAuth.On("ValidateCSRFToken", ctx, "sess-tok", "").Return(nil)
	mockAuth.On("HasPermissionAPI", ctx, "", "approve-any", "purchases").Return(false, nil).Maybe()
	mockAuth.On("HasPermissionAPI", ctx, "", "approve-own", "purchases").Return(false, nil).Maybe()

	mockPurchase := new(MockPurchaseManager)
	mockPurchase.On("ApproveExecution", ctx, execID, "valid-token", contact).Return(nil, freshToken, nil)

	notifier := &recordingExecutedNotifier{}
	handler := &Handler{
		purchase:      mockPurchase,
		config:        mockConfig,
		auth:          mockAuth,
		emailNotifier: notifier,
	}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer sess-tok"},
	}
	result, err := handler.approvePurchase(ctx, req, execID, "valid-token")
	require.NoError(t, err, "approve must still succeed even when the email re-fetch fails")
	assert.Equal(t, "completed", result.(map[string]string)["status"])

	// The notification still fires (best-effort) and, critically, still
	// carries a working revocation token -- unaffected by the re-fetch
	// failure since it never sourced the token from the re-fetch.
	require.Equal(t, 1, notifier.calls, "notification must still fire on the degraded path")
	assert.Equal(t, freshToken, notifier.captured.RevocationToken,
		"re-fetch failure must not affect the revocation token embedded in the email")

	mockPurchase.AssertExpectations(t)
	mockConfig.AssertExpectations(t)
}

// TestExecutedNotification_SessionApprovePath covers the dashboard
// (session-authed) approve branch via approvePurchaseViaSession: after
// ApproveAndExecute succeeds, the notification must fire.
func TestExecutedNotification_SessionApprovePath(t *testing.T) {
	ctx := context.Background()
	execID := "23456789-2345-2345-2345-23456789abcd"
	adminEmail := "admin@example.com"
	contact := "contact@example.com"
	freshToken := "session-revoke-token"

	mockConfig := new(MockConfigStore)
	t.Cleanup(func() { mockConfig.AssertExpectations(t) })
	exec := approvalTestExec(execID, contact, mockConfig)
	mockConfig.On("GetExecutionByID", ctx, execID).Return(exec, nil)
	mockConfig.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{
		NotificationEmail: &contact,
	}, nil)

	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("ValidateSession", ctx, "sess-tok").Return(&Session{Email: adminEmail}, nil)
	mockAuth.grantAdminPurchaser()
	mockAuth.On("ValidateCSRFToken", ctx, "sess-tok", "").Return(nil)

	mockPurchase := new(MockPurchaseManager)
	// The session path mints its own fresh revocation token too (issue #103):
	// once only the token hash is stored, reusing the pre-approve
	// execution's ApprovalToken (the pre-fix behavior) is no longer possible,
	// so ApproveAndExecute mints and returns one on every successful approve
	// regardless of which path triggered it.
	mockPurchase.On("ApproveAndExecute", ctx, execID, adminEmail, (*string)(nil)).Return(nil, freshToken, nil)

	notifier := &recordingExecutedNotifier{}
	handler := &Handler{
		purchase:      mockPurchase,
		config:        mockConfig,
		auth:          mockAuth,
		emailNotifier: notifier,
	}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer sess-tok"},
	}
	// Empty token forces the dashboard (session) branch.
	result, err := handler.approvePurchase(ctx, req, execID, "")
	require.NoError(t, err)
	assert.Equal(t, "completed", result.(map[string]string)["status"])

	// Admin approved, so the executor recorded in the body is the admin.
	assertExecutedNotificationFingerprints(t, notifier, contact, adminEmail, freshToken)
	mockPurchase.AssertExpectations(t)
	mockPurchase.AssertNotCalled(t, "ApproveExecution", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestExecutedNotification_DirectExecutePath is the regression test for the
// adversarial-verification blocker: the direct-execute path (issue #289) sent
// NO notification at all before the #291 wiring. After ApproveAndExecute
// succeeds, directExecutePurchase must fire the post-execution notification --
// this is the only email the direct-execute flow emits.
func TestExecutedNotification_DirectExecutePath(t *testing.T) {
	ctx := context.Background()
	execID := "34567890-3456-3456-3456-34567890abcd"
	adminEmail := "admin@example.com"
	contact := "contact@example.com"
	accountID := "acct-1"
	freshToken := "direct-execute-revoke-token"

	mockConfig := new(MockConfigStore)
	t.Cleanup(func() { mockConfig.AssertExpectations(t) })
	exec := &config.PurchaseExecution{
		ExecutionID:   execID,
		ApprovalToken: "valid-token",
		Status:        "pending",
		Recommendations: []config.RecommendationRecord{
			{ID: "r1", CloudAccountID: &accountID},
		},
	}
	mockConfig.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
		return &config.CloudAccount{ID: id, ContactEmail: contact}, nil
	}
	// DirectExecute stamps the audit fields after its claim (the handler
	// no longer saves the row itself), then sendPurchaseExecutedEmail
	// reads the global config.
	mockConfig.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{
		NotificationEmail: &contact,
	}, nil)

	mockPurchase := new(MockPurchaseManager)
	// Direct-execute also mints its own fresh revocation token (issue #103),
	// replacing the pre-fix "reuse execution.ApprovalToken directly" behavior
	// -- see the SessionApprovePath test above for the full rationale.
	mockPurchase.On("DirectExecute", ctx, execID, adminEmail, (*string)(nil)).Return(nil, freshToken, nil)

	notifier := &recordingExecutedNotifier{}
	handler := &Handler{
		purchase:      mockPurchase,
		config:        mockConfig,
		emailNotifier: notifier,
		// auth left nil: lookupRequesterInfo tolerates a nil auth and the
		// execution has no CreatedByUserID, so the requester lookup is skipped.
	}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer sess-tok"},
	}
	session := &Session{Email: adminEmail, UserID: "admin-uid"}

	result, err := handler.directExecutePurchase(ctx, req, exec, session, nil)
	require.NoError(t, err)
	resultMap := result.(map[string]any)
	assert.Equal(t, "completed", resultMap["status"])
	assert.Equal(t, true, resultMap["direct_execute"])

	assertExecutedNotificationFingerprints(t, notifier, contact, adminEmail, freshToken)
	mockPurchase.AssertExpectations(t)
}

// TestExecutedNotification_DirectExecute_NilNotifierNoPanic guards the
// best-effort contract: a direct-execute with no email notifier configured
// must still complete the purchase without panicking.
func TestExecutedNotification_DirectExecute_NilNotifierNoPanic(t *testing.T) {
	ctx := context.Background()
	execID := "45678901-4567-4567-4567-45678901abcd"
	adminEmail := "admin@example.com"

	mockConfig := new(MockConfigStore)
	t.Cleanup(func() { mockConfig.AssertExpectations(t) })
	exec := &config.PurchaseExecution{
		ExecutionID:   execID,
		ApprovalToken: "valid-token",
		Status:        "pending",
		Recommendations: []config.RecommendationRecord{
			{ID: "r1"},
		},
	}

	mockPurchase := new(MockPurchaseManager)
	mockPurchase.On("DirectExecute", ctx, execID, adminEmail, (*string)(nil)).Return(nil, "", nil)

	handler := &Handler{
		purchase:      mockPurchase,
		config:        mockConfig,
		emailNotifier: nil, // best-effort send is skipped
	}

	req := &events.LambdaFunctionURLRequest{}
	session := &Session{Email: adminEmail, UserID: "admin-uid"}

	result, err := handler.directExecutePurchase(ctx, req, exec, session, nil)
	require.NoError(t, err)
	assert.Equal(t, "completed", result.(map[string]any)["status"])
	mockPurchase.AssertExpectations(t)
}
