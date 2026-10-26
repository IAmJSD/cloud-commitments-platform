package api

import (
	"context"
	"errors"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
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

	status, err := h.finalizePurchaseStatus(context.Background(), exec, false, "send failed: throttled")

	require.NoError(t, err)
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

			status, err := h.finalizePurchaseStatus(context.Background(), exec, false, "send failed: throttled")

			require.NoError(t, err)
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

	status, err := h.finalizePurchaseStatus(context.Background(), newFinalizeExecution(), false, "x")
	require.NoError(t, err)
	assert.Equal(t, "pending", status)
	store.AssertExpectations(t)
}

// TestFinalizePurchaseStatus_WriteErrorSurfaces pins issue #54: when the
// failed write itself errors, the function must not report "pending" (the row
// would look like a healthy request waiting on its approver). It returns a
// 500 that names the execution and carries the email reason, keeps the store
// error reachable with errors.Is, and leaves the caller's copy untouched.
func TestFinalizePurchaseStatus_WriteErrorSurfaces(t *testing.T) {
	store := new(MockConfigStore)
	saveErr := errors.New("connection reset")
	store.On("SaveExecutionIfStatus", mock.Anything, mock.Anything, []string{"pending"}).
		Return(saveErr).Once()
	h := &Handler{config: store}
	exec := newFinalizeExecution()

	status, err := h.finalizePurchaseStatus(context.Background(), exec, false, "send failed: throttled")

	assert.Empty(t, status)
	requireEmailFailureNotRecorded(t, err, finalizeExecID, "send failed: throttled")
	assert.ErrorIs(t, err, saveErr)
	assert.Equal(t, "pending", exec.Status, "a failed write must not mutate the caller's copy")
	assert.Empty(t, exec.Error)
	store.AssertExpectations(t)
	store.AssertNotCalled(t, "GetExecutionByID", mock.Anything, mock.Anything)
}

// TestFinalizePurchaseStatus_MissingRowSurfaces: a row that vanished between
// the insert and the failed write (ErrNotFound from the guarded save) is a
// save failure too, not a lost guard, so it gets the same 500.
func TestFinalizePurchaseStatus_MissingRowSurfaces(t *testing.T) {
	store := new(MockConfigStore)
	store.On("SaveExecutionIfStatus", mock.Anything, mock.Anything, []string{"pending"}).
		Return(config.ErrNotFound).Once()
	h := &Handler{config: store}

	_, err := h.finalizePurchaseStatus(context.Background(), newFinalizeExecution(), false, "x")

	requireEmailFailureNotRecorded(t, err, finalizeExecID, "x")
	assert.ErrorIs(t, err, config.ErrNotFound)
	store.AssertNotCalled(t, "GetExecutionByID", mock.Anything, mock.Anything)
}

// requireEmailFailureNotRecorded asserts err is the issue #54 500 for
// executionID, and that the response body the router builds from it names
// the execution and the email reason but not the store error.
func requireEmailFailureNotRecorded(t *testing.T, err error, executionID, emailReason string) {
	t.Helper()
	require.Error(t, err, "a failed failure write must not be reported as a normal response")
	ce, ok := IsClientError(err)
	require.True(t, ok, "want a client-facing error, got %T: %v", err, err)
	assert.Equal(t, 500, ce.code)
	assert.Contains(t, ce.message, "AUDIT LOSS")
	assert.Contains(t, ce.message, executionID)

	code, body := (&Handler{}).handleRequestError(err)
	assert.Equal(t, 500, code)
	m, ok := body.(map[string]any)
	require.True(t, ok, "want a structured body, got %T", body)
	assert.Equal(t, executionID, m["execution_id"])
	assert.Equal(t, false, m["email_sent"])
	assert.Equal(t, emailReason, m["email_reason"])
	assert.NotContains(t, m["error"], "connection reset", "the store error must stay out of the response body")
}

// TestFinalizePurchaseStatus_EmailSentWritesNothing: a sent email leaves the
// row pending and touches the store not at all.
func TestFinalizePurchaseStatus_EmailSentWritesNothing(t *testing.T) {
	store := new(MockConfigStore)
	h := &Handler{config: store}
	status, err := h.finalizePurchaseStatus(context.Background(), newFinalizeExecution(), true, "")
	require.NoError(t, err)
	require.Equal(t, "pending", status)
	store.AssertNotCalled(t, "SaveExecutionIfStatus", mock.Anything, mock.Anything, mock.Anything)
	store.AssertNotCalled(t, "SavePurchaseExecution", mock.Anything, mock.Anything)
}

// TestExecutePurchase_EmailFailureNotRecorded_Returns500 drives the submit
// path (issue #54): no notifier is wired, so the approval email fails, and
// the guarded write recording that failure errors too. Before the fix the
// handler answered 200 {"status":"pending","email_sent":false}.
func TestExecutePurchase_EmailFailureNotRecorded_Returns500(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	authSvc := new(MockAuthService)
	authSvc.On("ValidateSession", ctx, "admin-token").Return(&Session{UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Email: "admin@example.com"}, nil)
	authSvc.grantAdminPurchaser()
	var inserted *config.PurchaseExecution
	store.On("SavePurchaseExecution", ctx, mock.AnythingOfType("*config.PurchaseExecution")).
		Run(func(args mock.Arguments) { inserted = args.Get(1).(*config.PurchaseExecution) }).
		Return(nil).Once()
	store.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{}, nil)
	store.On("GetPendingExecutions", ctx).Return([]config.PurchaseExecution{}, nil)
	expectStoredRecs(store, config.RecommendationRecord{Provider: "aws", Service: "ec2", ResourceType: "m5.large", Count: 1, Term: 1, Payment: "all-upfront", UpfrontCost: 100, Savings: 50})
	saveErr := errors.New("connection reset")
	store.On("SaveExecutionIfStatus", ctx, mock.Anything, []string{"pending"}).Return(saveErr).Once()
	h := &Handler{config: store, auth: authSvc}
	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"Authorization": "Bearer admin-token"},
		Body:    `{"recommendations": [{"id": "", "provider": "aws", "service": "ec2", "resource_type": "m5.large", "count": 1, "term": 1, "payment": "all-upfront", "upfront_cost": 100.0, "savings": 50.0}]}`,
	}

	result, err := h.executePurchase(ctx, req)

	assert.Nil(t, result, "no success body may accompany the error")
	require.NotNil(t, inserted, "the pending row must have been inserted before the send")
	requireEmailFailureNotRecorded(t, err, inserted.ExecutionID, "email notifier not configured for this deployment")
	assert.ErrorIs(t, err, saveErr)
	store.AssertExpectations(t)
}

// TestRetryPurchase_EmailFailureNotRecorded_Returns500 is the retry-path
// twin: the successor row is inserted and linked, its approval email fails,
// and recording that failure errors. The response must be the 500 naming the
// successor, not a 200 that reports it as pending.
func TestRetryPurchase_EmailFailureNotRecorded_Returns500(t *testing.T) {
	creator := retryOtherID
	failedExec := &config.PurchaseExecution{
		ExecutionID:     retryExecID,
		Status:          "failed",
		Error:           "send failed: transient SES throttle",
		CreatedByUserID: &creator,
		Recommendations: []config.RecommendationRecord{{Provider: "aws", Service: "rds", Term: 1, UpfrontCost: 100}},
	}
	h, store, _ := buildSessionRetryHandler(failedExec, &Session{UserID: retryCallerID, Email: "admin@example.com"}, true, false)
	var successor *config.PurchaseExecution
	store.On("SavePurchaseExecution", mock.Anything, mock.AnythingOfType("*config.PurchaseExecution")).
		Run(func(args mock.Arguments) { successor = args.Get(1).(*config.PurchaseExecution) }).
		Return(nil).Once()
	store.On("LinkRetryExecutionAtomic", mock.Anything, mock.Anything, retryExecID, mock.AnythingOfType("string")).Return(true, nil).Once()
	saveErr := errors.New("connection reset")
	store.On("SaveExecutionIfStatus", mock.Anything, mock.Anything, []string{"pending"}).Return(saveErr).Once()

	result, err := h.retryPurchase(context.Background(), sessionRetryReq(), retryExecID)

	assert.Nil(t, result, "no success body may accompany the error")
	require.NotNil(t, successor, "the successor row must have been inserted before the send")
	requireEmailFailureNotRecorded(t, err, successor.ExecutionID, "email notifier not configured for this deployment")
	assert.ErrorIs(t, err, saveErr)
	store.AssertExpectations(t)
}
