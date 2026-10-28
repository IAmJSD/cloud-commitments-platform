package api

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #532: a partially_completed run bought part of its commitments, so it
// is revocable. The funnel mints a revocation token for it and the handler
// emails it like a completed run, then still answers 502 with the row status.

const partialRunToken = "partial-revoke-token"

var errPartialRun = errors.New("some purchases failed: m5.large: provider rejected the purchase")

// partialRunRow is the row the funnel returns for a single-account partial
// run: one rec bought, one failed, completed_at stamped, the token's hash
// stored.
func partialRunRow(execID string) *config.PurchaseExecution {
	accountID := "acct-1"
	completed := time.Now().Add(-time.Minute)
	return &config.PurchaseExecution{
		ExecutionID:   execID,
		Status:        config.StatusPartiallyCompleted,
		CompletedAt:   &completed,
		ApprovalToken: config.HashApprovalToken(partialRunToken),
		Recommendations: []config.RecommendationRecord{
			{ID: "r1", Provider: "aws", Service: "ec2", ResourceType: "m5.xlarge", Region: "us-east-1", CloudAccountID: &accountID, UpfrontCost: 100, Savings: 10, Purchased: true},
			{ID: "r2", Provider: "aws", Service: "ec2", ResourceType: "m5.large", Region: "us-east-1", CloudAccountID: &accountID, UpfrontCost: 50, Savings: 5},
		},
		TotalUpfrontCost: 150,
		EstimatedSavings: 15,
	}
}

// assertPartialRunEmailed pins the executed email of a partial run: sent once,
// carrying the minted token (valid against the stored hash), marked partial,
// and listing only the commitment that was bought, with its own totals.
func assertPartialRunEmailed(t *testing.T, n *recordingExecutedNotifier, row *config.PurchaseExecution) {
	t.Helper()
	require.Equal(t, 1, n.calls, "a partial run must send the executed email")
	assert.Equal(t, partialRunToken, n.captured.RevocationToken)
	assert.NoError(t, validateRevokeToken(row, n.captured.RevocationToken), "the emailed token must revoke the row")
	assert.True(t, n.captured.PartialPurchase)
	require.Len(t, n.captured.Recommendations, 1, "only the bought commitment is listed")
	assert.Equal(t, "m5.xlarge", n.captured.Recommendations[0].ResourceType)
	assert.InDelta(t, 100.0, n.captured.TotalUpfrontCost, 0.001)
	assert.InDelta(t, 10.0, n.captured.TotalSavings, 0.001)
	assert.NotEmpty(t, n.captured.RevocationWindowClosesAt)
}

func assertPartialRun502(t *testing.T, err error) {
	t.Helper()
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a client error, got %v", err)
	assert.Equal(t, 502, ce.code)
	assert.Contains(t, ce.message, "partially completed")
	assert.Equal(t, config.StatusPartiallyCompleted, ce.Details()["status"])
}

func sessionApproveHandler(ctx context.Context, execID, contact string, final *config.PurchaseExecution, token string, runErr error) (*Handler, *recordingExecutedNotifier) {
	mockConfig := new(MockConfigStore)
	exec := approvalTestExec(execID, contact, mockConfig)
	mockConfig.On("GetExecutionByID", ctx, execID).Return(exec, nil)
	mockConfig.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{NotificationEmail: &contact}, nil)
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", ctx, "sess-tok").Return(&Session{Email: "admin@example.com"}, nil)
	mockAuth.grantAdminPurchaser()
	mockAuth.On("ValidateCSRFToken", ctx, "sess-tok", "").Return(nil)
	mockPurchase := new(MockPurchaseManager)
	mockPurchase.On("ApproveAndExecute", ctx, execID, "admin@example.com", (*string)(nil)).Return(final, token, runErr)
	notifier := &recordingExecutedNotifier{}
	return &Handler{purchase: mockPurchase, config: mockConfig, auth: mockAuth, emailNotifier: notifier}, notifier
}

func TestExecutedNotification_PartialSessionApproveEmailsRevocationToken(t *testing.T) {
	ctx := context.Background()
	execID := "56789012-5678-5678-5678-56789012abcd"
	row := partialRunRow(execID)
	handler, notifier := sessionApproveHandler(ctx, execID, "contact@example.com", row, partialRunToken, errPartialRun)

	_, err := handler.approvePurchase(ctx, &events.LambdaFunctionURLRequest{Headers: map[string]string{"authorization": "Bearer sess-tok"}}, execID, "")
	assertPartialRun502(t, err)
	assertPartialRunEmailed(t, notifier, row)
}

// Nothing committed (failed), or the partial status never reached the store
// (audit loss): no revocable purchase exists, so no executed email.
func TestExecutedNotification_UncommittedFailureSendsNoEmail(t *testing.T) {
	ctx := context.Background()
	execID := "67890123-6789-6789-6789-67890123abcd"
	failed := partialRunRow(execID)
	failed.Status = "failed"
	cases := map[string]struct {
		final *config.PurchaseExecution
		err   error
	}{
		"failed run":         {failed, errors.New("AWS RI purchase failed")},
		"partial audit loss": {partialRunRow(execID), fmt.Errorf("%w: terminal save failed; original execution error: %w", config.ErrAuditLoss, errPartialRun)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			handler, notifier := sessionApproveHandler(ctx, execID, "contact@example.com", tc.final, "", tc.err)
			_, err := handler.approvePurchase(ctx, &events.LambdaFunctionURLRequest{Headers: map[string]string{"authorization": "Bearer sess-tok"}}, execID, "")
			require.Error(t, err)
			assert.Zero(t, notifier.calls)
		})
	}
}

func TestExecutedNotification_PartialDirectExecuteEmailsRevocationToken(t *testing.T) {
	ctx := context.Background()
	execID := "78901234-7890-7890-7890-78901234abcd"
	contact := "contact@example.com"
	row := partialRunRow(execID)

	mockConfig := new(MockConfigStore)
	mockConfig.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
		return &config.CloudAccount{ID: id, ContactEmail: contact}, nil
	}
	mockConfig.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{NotificationEmail: &contact}, nil)
	mockPurchase := new(MockPurchaseManager)
	mockPurchase.On("DirectExecute", ctx, execID, "admin@example.com", (*string)(nil)).Return(row, partialRunToken, errPartialRun)
	notifier := &recordingExecutedNotifier{}
	handler := &Handler{purchase: mockPurchase, config: mockConfig, emailNotifier: notifier}

	pending := &config.PurchaseExecution{ExecutionID: execID, Status: "pending", Recommendations: row.Recommendations}
	_, err := handler.directExecutePurchase(ctx, &events.LambdaFunctionURLRequest{}, pending, &Session{Email: "admin@example.com", UserID: "admin-uid"}, nil)
	assertPartialRun502(t, err)
	assertPartialRunEmailed(t, notifier, row)
}

// tokenApproveHandler wires the email-link approve: the caller is the
// account's contact mailbox with no approve-* permission, so approvePurchase
// takes the token branch, whose ApproveExecution returns final, token and
// runErr. The execution is read once, for the request itself: a failed run
// answers from the returned row, never from a re-fetch.
func tokenApproveHandler(ctx context.Context, execID, contact string, final *config.PurchaseExecution, token string, runErr error) (*Handler, *recordingExecutedNotifier, *MockConfigStore) {
	mockConfig := new(MockConfigStore)
	exec := approvalTestExec(execID, contact, mockConfig)
	mockConfig.On("GetExecutionByID", ctx, execID).Return(exec, nil).Once()
	mockConfig.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{NotificationEmail: &contact}, nil)
	mockAuth := new(MockAuthService)
	mockAuth.On("GetAllowedAccountsAPI", mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	mockAuth.On("ValidateSession", ctx, "sess-tok").Return(&Session{Email: contact}, nil)
	mockAuth.On("ValidateCSRFToken", ctx, "sess-tok", "").Return(nil)
	mockAuth.On("HasPermissionAPI", ctx, "", "approve-any", "purchases").Return(false, nil).Maybe()
	mockAuth.On("HasPermissionAPI", ctx, "", "approve-own", "purchases").Return(false, nil).Maybe()
	mockPurchase := new(MockPurchaseManager)
	mockPurchase.On("ApproveExecution", ctx, execID, "valid-token", contact).Return(final, token, runErr)
	notifier := &recordingExecutedNotifier{}
	return &Handler{purchase: mockPurchase, config: mockConfig, auth: mockAuth, emailNotifier: notifier}, notifier, mockConfig
}

// The token approve now gets the row back, so a partial run answers like the
// session approve: emailed from the returned row, then a 502 with its status.
func TestExecutedNotification_PartialTokenApproveEmailsRevocationToken(t *testing.T) {
	ctx := context.Background()
	execID := "89012345-8901-8901-8901-89012345abcd"
	row := partialRunRow(execID)
	handler, notifier, mockConfig := tokenApproveHandler(ctx, execID, "contact@example.com", row, partialRunToken, errPartialRun)

	_, err := handler.approvePurchase(ctx, &events.LambdaFunctionURLRequest{Headers: map[string]string{"authorization": "Bearer sess-tok"}}, execID, "valid-token")
	assertPartialRun502(t, err)
	assertPartialRunEmailed(t, notifier, row)
	mockConfig.AssertExpectations(t)
}

// Issue #532 slice 3: a token approve maps its failures by cause, as the
// session approve does. Past the claim (a row came back) a provider failure
// is a 502 with the row's status and an audit loss a 500; before it, the
// 4-eyes denial is a 403 and the token and lost-claim refusals keep their
// 4xx. Nothing here committed, so nothing is emailed.
func TestTokenApprove_FailureMappedByCause(t *testing.T) {
	ctx := context.Background()
	execID := "a1234567-a123-a123-a123-a1234567abcd"
	failed := partialRunRow(execID)
	failed.Status = "failed"
	cases := map[string]struct {
		final  *config.PurchaseExecution
		err    error
		code   int // 0: a plain (500) error, not a client error
		status string
	}{
		"provider failure after the claim": {failed, errors.New("AWS RI purchase failed"), 502, "failed"},
		"final status not saved":           {partialRunRow(execID), fmt.Errorf("%w: terminal save failed; original execution error: %w", config.ErrAuditLoss, errPartialRun), 500, ""},
		"4-eyes denial before the claim":   {nil, fmt.Errorf("%w: approver is the requester", purchase.ErrFourEyesDenied), 403, ""},
		"lost claim":                       {nil, fmt.Errorf("approve: %w", config.ErrExecutionNotInExpectedStatus), 409, ""},
		"token rotated before the claim":   {nil, fmt.Errorf("approve: %w", config.ErrApprovalTokenChanged), 403, ""},
		"not started, store error":         {nil, errors.New("failed to get execution: db down"), 0, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			handler, notifier, _ := tokenApproveHandler(ctx, execID, "contact@example.com", tc.final, "", tc.err)
			_, err := handler.approvePurchase(ctx, &events.LambdaFunctionURLRequest{Headers: map[string]string{"authorization": "Bearer sess-tok"}}, execID, "valid-token")
			require.Error(t, err)
			assert.Zero(t, notifier.calls)
			ce, ok := IsClientError(err)
			if tc.code == 0 {
				assert.False(t, ok, "a failure before the run started stays a plain error, got %v", err)
				return
			}
			require.True(t, ok, "expected a client error, got %v", err)
			assert.Equal(t, tc.code, ce.code)
			if tc.status != "" {
				assert.Equal(t, execID, ce.Details()["execution_id"])
				assert.Equal(t, tc.status, ce.Details()["status"])
			}
		})
	}
}

func TestExecutedNotification_TokenApproveFailureWithoutTokenSendsNoEmail(t *testing.T) {
	ctx := context.Background()
	execID := "90123456-9012-9012-9012-90123456abcd"
	contact := "contact@example.com"

	mockConfig := new(MockConfigStore)
	exec := approvalTestExec(execID, contact, mockConfig)
	mockConfig.On("GetExecutionByID", ctx, execID).Return(exec, nil).Once()
	mockConfig.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{NotificationEmail: &contact}, nil)
	mockAuth := new(MockAuthService)
	mockAuth.On("GetAllowedAccountsAPI", mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	mockAuth.On("ValidateSession", ctx, "sess-tok").Return(&Session{Email: contact}, nil)
	mockAuth.On("ValidateCSRFToken", ctx, "sess-tok", "").Return(nil)
	mockAuth.On("HasPermissionAPI", ctx, "", "approve-any", "purchases").Return(false, nil).Maybe()
	mockAuth.On("HasPermissionAPI", ctx, "", "approve-own", "purchases").Return(false, nil).Maybe()
	mockPurchase := new(MockPurchaseManager)
	mockPurchase.On("ApproveExecution", ctx, execID, "valid-token", contact).Return(nil, "", errors.New("AWS RI purchase failed"))
	notifier := &recordingExecutedNotifier{}
	handler := &Handler{purchase: mockPurchase, config: mockConfig, auth: mockAuth, emailNotifier: notifier}

	_, err := handler.approvePurchase(ctx, &events.LambdaFunctionURLRequest{Headers: map[string]string{"authorization": "Bearer sess-tok"}}, execID, "valid-token")
	require.Error(t, err)
	assert.Zero(t, notifier.calls)
	mockConfig.AssertExpectations(t)
}

// A fan-out root records no per-rec Purchased flags (its per-account rows
// do), so its partial email lists every rec with the row totals; the partial
// notice carries the caveat.
func TestExecutedEmailCommitments(t *testing.T) {
	row := partialRunRow("e1")
	recs, upfront, savings := executedEmailCommitments(row)
	assert.Len(t, recs, 1)
	assert.InDelta(t, 100.0, upfront, 0.001)
	assert.InDelta(t, 10.0, savings, 0.001)

	root := partialRunRow("e2")
	for i := range root.Recommendations {
		root.Recommendations[i].Purchased = false
	}
	recs, upfront, savings = executedEmailCommitments(root)
	assert.Len(t, recs, 2)
	assert.InDelta(t, 150.0, upfront, 0.001)
	assert.InDelta(t, 15.0, savings, 0.001)

	completed := partialRunRow("e3")
	completed.Status = config.StatusCompleted
	recs, upfront, _ = executedEmailCommitments(completed)
	assert.Len(t, recs, 2, "a completed row lists every rec, as before")
	assert.InDelta(t, 150.0, upfront, 0.001)
}
