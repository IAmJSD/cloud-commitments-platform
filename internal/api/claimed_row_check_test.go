package api

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #415, handler side: each execute entry point checks Constraints on the
// row GetExecutionByID returned, then hands the purchase funnel a
// ClaimedRowCheck that re-runs them on the row the claim CAS returns.
// MockPurchaseManager.ClaimedRow plays that CAS-returned row (see its doc);
// the Constraints are decided by the real auth matcher (grantPermissions).
// The funnel side (provider never called, row failed) is pinned in
// internal/purchase/claimed_row_check_test.go and, against real Postgres, in
// handler_purchases_claimed_row_integration_test.go.

const (
	claimCap      = 1000.0
	claimUnder    = 400.0  // what the handler's pre-CAS check saw
	claimRepriced = 5000.0 // what the CAS returned
)

func claimRecs(upfront float64) []config.RecommendationRecord {
	return []config.RecommendationRecord{{ID: "r1", Provider: "aws", Service: "ec2", Region: "us-east-1", UpfrontCost: upfront, CommitmentCurrency: "USD"}}
}

func cappedPurchasePerm(action string) auth.Permission {
	return auth.Permission{Action: action, Resource: auth.ResourcePurchases, Constraints: &auth.PermissionConstraints{MaxPurchaseAmount: claimCap}}
}

// repriced returns a copy of exec whose recommendations cost upfront.
func repriced(exec *config.PurchaseExecution, upfront float64) *config.PurchaseExecution {
	c := *exec
	c.Recommendations = claimRecs(upfront)
	return &c
}

// requireClaimedRowRefused asserts the 403 a post-claim refusal maps to and
// that the handler handed the funnel exactly one non-nil check.
func requireClaimedRowRefused(t *testing.T, mp *MockPurchaseManager, err error) {
	t.Helper()
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a clientError, got: %v", err)
	assert.Equal(t, 403, ce.code)
	assert.Contains(t, ce.message, "nothing was bought")
	assert.Contains(t, ce.message, "constraints")
	assert.Equal(t, "failed", ce.Details()["status"])
	requireOneCheck(t, mp)
}

func requireOneCheck(t *testing.T, mp *MockPurchaseManager) {
	t.Helper()
	require.Len(t, mp.Checks, 1)
	require.NotNil(t, mp.Checks[0], "every session entry point must pass a ClaimedRowCheck")
}

// --- Run now ---------------------------------------------------------------

func runNowClaimHarness(t *testing.T, mp *MockPurchaseManager, claimedUpfront float64) error {
	t.Helper()
	ctx := context.Background()
	execID := "11111111-1111-1111-1111-111111111111"
	snapshot := &config.PurchaseExecution{ExecutionID: execID, Status: "pending", Recommendations: claimRecs(claimUnder)}
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockStore.On("GetExecutionByID", ctx, execID).Return(snapshot, nil)
	mockStore.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{}, nil).Maybe()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("ValidateSession", ctx, "capped-token").Return(&Session{UserID: "dddddddd-dddd-dddd-dddd-dddddddddddd", Email: "capped@example.com"}, nil)
	mockAuth.grantPermissions([]auth.Permission{
		{Action: auth.ActionAdmin, Resource: auth.ResourceAll},
		cappedPurchasePerm(auth.ActionExecute),
	})
	mp.ClaimedRow = repriced(snapshot, claimedUpfront)
	mp.On("RunPlannedPurchaseNow", ctx, execID, mock.Anything, mock.Anything).
		Return(&config.PurchaseExecution{ExecutionID: execID, Status: "completed"}, "", nil).Maybe()
	handler := &Handler{purchase: mp, config: mockStore, auth: mockAuth}
	req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer capped-token"}}
	_, err := handler.runPlannedPurchase(ctx, req, execID)
	return err
}

func TestClaimedRowCheck_RunNow_RepricedRowRefused(t *testing.T) {
	mp := &MockPurchaseManager{}
	t.Cleanup(func() { mp.AssertExpectations(t) })
	err := runNowClaimHarness(t, mp, claimRepriced)
	requireClaimedRowRefused(t, mp, err)
	mp.AssertNotCalled(t, "RunPlannedPurchaseNow", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestClaimedRowCheck_RunNow_UnmodifiedRowRuns(t *testing.T) {
	mp := &MockPurchaseManager{}
	t.Cleanup(func() { mp.AssertExpectations(t) })
	err := runNowClaimHarness(t, mp, claimUnder)
	require.NoError(t, err)
	requireOneCheck(t, mp)
	mp.AssertNumberOfCalls(t, "RunPlannedPurchaseNow", 1)
}

// --- Session approve -------------------------------------------------------

func sessionApproveClaimHarness(t *testing.T, mp *MockPurchaseManager, claimedUpfront float64) error {
	t.Helper()
	ctx := context.Background()
	execID := "abcdefab-cdef-abcd-efab-cdefabcdef01"
	approver := "approver@example.com"
	snapshot := &config.PurchaseExecution{ExecutionID: execID, Status: "pending", Recommendations: claimRecs(claimUnder)}
	mockConfig := new(MockConfigStore)
	t.Cleanup(func() { mockConfig.AssertExpectations(t) })
	mockConfig.On("GetExecutionByID", ctx, execID).Return(snapshot, nil)
	mockConfig.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{}, nil)
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("ValidateSession", ctx, "sess-tok").Return(&Session{UserID: "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee", Email: approver}, nil)
	mockAuth.On("ValidateCSRFToken", ctx, "sess-tok", "").Return(nil)
	mockAuth.grantPermissions([]auth.Permission{cappedPurchasePerm(auth.ActionApproveAny)})
	mp.ClaimedRow = repriced(snapshot, claimedUpfront)
	mp.On("ApproveAndExecute", ctx, execID, approver, mock.Anything).Return(nil, "", nil).Maybe()
	handler := &Handler{purchase: mp, config: mockConfig, auth: mockAuth}
	req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"authorization": "Bearer sess-tok"}}
	_, err := handler.approvePurchase(ctx, req, execID, "")
	return err
}

func TestClaimedRowCheck_SessionApprove_RepricedRowRefused(t *testing.T) {
	mp := &MockPurchaseManager{}
	t.Cleanup(func() { mp.AssertExpectations(t) })
	err := sessionApproveClaimHarness(t, mp, claimRepriced)
	requireClaimedRowRefused(t, mp, err)
	mp.AssertNotCalled(t, "ApproveAndExecute", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestClaimedRowCheck_SessionApprove_UnmodifiedRowRuns(t *testing.T) {
	mp := &MockPurchaseManager{}
	t.Cleanup(func() { mp.AssertExpectations(t) })
	err := sessionApproveClaimHarness(t, mp, claimUnder)
	require.NoError(t, err)
	requireOneCheck(t, mp)
	mp.AssertNumberOfCalls(t, "ApproveAndExecute", 1)
}

// --- Token (email-link) approve ---------------------------------------------

// The approver is the account's contact_email holding a capped approve-own on
// a plan-step row; the plan cap variant holds no verb at all.
func TestClaimedRowCheck_TokenApprove(t *testing.T) {
	exec, total := planStepExecution(t)
	overCap := func(e *config.PurchaseExecution) *config.PurchaseExecution {
		c := *e
		c.Recommendations = append([]config.RecommendationRecord(nil), e.Recommendations...)
		c.Recommendations[0].Count *= 10
		c.Recommendations[0].UpfrontCost = total * 10
		return &c
	}
	for _, tc := range []struct {
		name  string
		perms []auth.Permission
		plan  *config.PurchasePlan
	}{
		{"approve-own cap", []auth.Permission{cappedPerm(auth.ActionApproveOwn, total)}, autoPlan(nil)},
		{"approve-any cap", []auth.Permission{cappedPerm(auth.ActionApproveAny, total)}, autoPlan(nil)},
		{"no verb, plan step cap", nil, autoPlan(&total)},
	} {
		t.Run(tc.name+"/repriced row refused", func(t *testing.T) {
			f := newTokenApprove(t, tc.perms, tc.plan)
			f.purchase.ClaimedRow = overCap(exec)
			requireClaimedRowRefusedAnyBody(t, f.purchase, f.approve(exec))
			assert.Zero(t, approveCalls(f.purchase), "the purchase must not run")
		})
		t.Run(tc.name+"/unmodified row runs", func(t *testing.T) {
			f := newTokenApprove(t, tc.perms, tc.plan)
			f.purchase.ClaimedRow = exec
			requireApproveReached(t, f.purchase, f.approve(exec))
			requireOneCheck(t, f.purchase)
		})
	}
}

// requireClaimedRowRefusedAnyBody is requireClaimedRowRefused for the token
// path, whose error carries no row (ApproveExecution returns none) and whose
// plan-cap refusal names the step cap rather than "constraints".
func requireClaimedRowRefusedAnyBody(t *testing.T, mp *MockPurchaseManager, err error) {
	t.Helper()
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a clientError, got: %v", err)
	assert.Equal(t, 403, ce.code)
	assert.Contains(t, ce.message, "nothing was bought")
	requireOneCheck(t, mp)
}

// --- Direct execute ----------------------------------------------------------

func directExecuteClaimHarness(t *testing.T, mp *MockPurchaseManager, claimedUpfront float64) error {
	t.Helper()
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	session := &Session{UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Email: "direct@example.com"}
	mockAuth.On("ValidateSession", ctx, "admin-token").Return(session, nil)
	mockAuth.grantPermissions([]auth.Permission{
		cappedPurchasePerm(auth.ActionExecute),
		cappedPurchasePerm(auth.ActionExecuteAny),
	})
	setupDirectExecMocks(ctx, mockStore)
	// directExecRecBody's rec-1 is priced from the stored set at 500, under the cap.
	mp.ClaimedRow = &config.PurchaseExecution{
		Status: "pending",
		Recommendations: []config.RecommendationRecord{{
			ID: "rec-1", Provider: "aws", Service: "ec2", Region: "us-east-1", Count: 1, Term: 1, Payment: "all-upfront", UpfrontCost: claimedUpfront,
			CommitmentCurrency: "USD",
		}},
	}
	mp.On("DirectExecute", ctx, mock.AnythingOfType("string"), session.Email, mock.Anything).Return(nil, "", nil).Maybe()
	handler := &Handler{config: mockStore, auth: mockAuth, purchase: mp}
	req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer admin-token"}, Body: directExecRecBody}
	_, err := handler.executePurchase(ctx, req)
	return err
}

func TestClaimedRowCheck_DirectExecute_RepricedRowRefused(t *testing.T) {
	mp := &MockPurchaseManager{}
	t.Cleanup(func() { mp.AssertExpectations(t) })
	err := directExecuteClaimHarness(t, mp, claimRepriced)
	requireClaimedRowRefused(t, mp, err)
	mp.AssertNotCalled(t, "DirectExecute", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestClaimedRowCheck_DirectExecute_UnmodifiedRowRuns(t *testing.T) {
	mp := &MockPurchaseManager{}
	t.Cleanup(func() { mp.AssertExpectations(t) })
	err := directExecuteClaimHarness(t, mp, 500)
	require.NoError(t, err)
	requireOneCheck(t, mp)
	mp.AssertNumberOfCalls(t, "DirectExecute", 1)
}

// --- Error mapping -------------------------------------------------------------

// A check that could not be evaluated (no clientError inside) is a server
// error, not a 403 or a 502: the row failed closed and nothing was bought.
func TestClaimedRowRefusalError_NonClientCauseIsServerError(t *testing.T) {
	err := claimedRowRefusalError(nil, assert.AnError)
	_, ok := IsClientError(err)
	assert.False(t, ok)
	assert.ErrorIs(t, err, assert.AnError)
}
