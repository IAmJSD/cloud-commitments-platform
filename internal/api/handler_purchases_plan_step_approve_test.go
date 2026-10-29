package api

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/planstep"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// These tests use in-memory fakes for the stores and the purchase manager
// (fixture/mock evidence). The recommendation under approval is the real
// output of planstep.Resolve, and the permission Constraints are evaluated by
// the real auth matcher.

type stepSource struct{ shown []config.RecommendationRecord }

func (s stepSource) ListRecommendationsStrict(context.Context, config.RecommendationFilter) ([]config.RecommendationRecord, error) {
	return s.shown, nil
}

type stepStore struct{ rows []config.RecommendationRecord }

func (s stepStore) ListStoredRecommendations(context.Context, config.RecommendationFilter) ([]config.RecommendationRecord, error) {
	return s.rows, nil
}
func (s stepStore) GetGlobalConfig(context.Context) (*config.GlobalConfig, error) {
	return &config.GlobalConfig{}, nil
}

const (
	stepPlanID  = "21212121-2121-2121-2121-212121212121"
	stepExecID  = "31313131-3131-3131-3131-313131313131"
	stepAcct    = "acct-step"
	stepContact = "contact@example.com"
	stepUser    = "41414141-4141-4141-4141-414141414141"
)

// planStepExecution builds the pending row a plan-step attach produces: the
// recs come from planstep.Resolve, and there is no CreatedByUserID.
func planStepExecution(t *testing.T) (*config.PurchaseExecution, float64) {
	t.Helper()
	acct := stepAcct
	monthly := 100.0
	stored := config.RecommendationRecord{
		ID: "azure|acct-step|compute|westeurope|Standard_D2s_v3||1|monthly", Provider: "azure", Service: "compute",
		Region: "westeurope", ResourceType: "Standard_D2s_v3", Term: 1, Payment: "monthly",
		Count: 20, UpfrontCost: 0, MonthlyCost: &monthly, CloudAccountID: &acct,
		CommitmentCurrency: "USD",
	}
	plan := &config.PurchasePlan{
		ID:           stepPlanID,
		Services:     map[string]config.ServiceConfig{"azure:compute": {Provider: "azure", Service: "compute", Enabled: true, Term: 1, Payment: "no-upfront", Coverage: 80}},
		RampSchedule: config.RampSchedule{PercentPerStep: 25},
	}
	res, err := planstep.Resolve(context.Background(), stepSource{[]config.RecommendationRecord{stored}}, stepStore{[]config.RecommendationRecord{stored}}, plan, []string{stepAcct}, 1, nil, nil)
	recs := res.Recs
	require.NoError(t, err)
	require.Len(t, recs, 1)
	total := recs[0].TotalCommitment()
	require.Greater(t, total, 0.0)
	return &config.PurchaseExecution{
		ExecutionID: stepExecID, PlanID: stepPlanID, ApprovalToken: "valid-token", Status: "pending",
		StepNumber: 1, Recommendations: recs,
	}, total
}

func cappedPerm(action string, maxAmount float64) auth.Permission {
	return auth.Permission{Action: action, Resource: auth.ResourcePurchases, Constraints: &auth.PermissionConstraints{MaxPurchaseAmount: maxAmount}}
}

type tokenApproveFixture struct {
	handler  *Handler
	purchase *MockPurchaseManager
	req      *events.LambdaFunctionURLRequest
	wired    bool
}

// newTokenApprove wires an approver who is the account's contact_email and
// holds perms (a nil list holds no approve verb).
func newTokenApprove(t *testing.T, perms []auth.Permission, plan *config.PurchasePlan) *tokenApproveFixture {
	t.Helper()
	mockConfig := new(MockConfigStore)
	t.Cleanup(func() { mockConfig.AssertExpectations(t) })
	mockConfig.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
		return &config.CloudAccount{ID: id, ContactEmail: stepContact}, nil
	}
	mockConfig.GetPurchasePlanFn = func(context.Context, string) (*config.PurchasePlan, error) { return plan, nil }
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.grantPermissions(perms)
	mp := new(MockPurchaseManager)
	// A reached ApproveExecution proves the approval was not refused up front;
	// it answers with a conflict so the test needs no email or re-fetch wiring.
	mp.On("ApproveExecution", mock.Anything, stepExecID, "valid-token", stepContact).Return(nil, "", config.ErrExecutionNotInExpectedStatus).Maybe()
	return &tokenApproveFixture{
		handler:  &Handler{purchase: mp, config: mockConfig, auth: mockAuth},
		purchase: mp,
		req:      &events.LambdaFunctionURLRequest{Headers: map[string]string{"authorization": "Bearer tok"}},
	}
}

// approve drives the token approval. Only this path resolves the session and
// reads the global config, so their expectations are registered here rather
// than in newTokenApprove, whose callers may test approveConstraintsForSession
// directly.
func (f *tokenApproveFixture) approve(exec *config.PurchaseExecution) error {
	ctx := context.Background()
	if !f.wired {
		f.wired = true
		f.handler.config.(*MockConfigStore).On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{}, nil)
		f.handler.auth.(*MockAuthService).On("ValidateSession", ctx, "tok").Return(&Session{UserID: stepUser, Email: stepContact}, nil)
	}
	_, err := f.handler.approveViaToken(ctx, f.req, exec, "valid-token")
	return err
}

func requireStatus(t *testing.T, err error, status int) {
	t.Helper()
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a clientError, got: %v", err)
	assert.Equal(t, status, ce.code)
}

func approveCalls(mp *MockPurchaseManager) int {
	n := 0
	for _, c := range mp.Calls {
		if c.Method == "ApproveExecution" {
			n++
		}
	}
	return n
}

func requireApproveReached(t *testing.T, mp *MockPurchaseManager, err error) {
	t.Helper()
	requireStatus(t, err, 409)
	assert.Equal(t, 1, approveCalls(mp), "approval should have been attempted")
}

func requireApproveRefused(t *testing.T, mp *MockPurchaseManager, err error, status int) {
	t.Helper()
	requireStatus(t, err, status)
	assert.Zero(t, approveCalls(mp), "approval must be refused before the purchase manager")
}

func autoPlan(cap *float64) *config.PurchasePlan {
	return &config.PurchasePlan{ID: stepPlanID, AutoPurchase: true, MaxStepCommitment: cap}
}

func TestApproveViaToken_PlanStep_ApproveOwnCappedContactEmail(t *testing.T) {
	exec, total := planStepExecution(t)

	over := newTokenApprove(t, []auth.Permission{cappedPerm(auth.ActionApproveOwn, total-1)}, autoPlan(nil))
	requireApproveRefused(t, over.purchase, over.approve(exec), 403)

	atCap := newTokenApprove(t, []auth.Permission{cappedPerm(auth.ActionApproveOwn, total)}, autoPlan(nil))
	requireApproveReached(t, atCap.purchase, atCap.approve(exec))
}

func TestApproveViaToken_PlanStep_ApproveAnyCap(t *testing.T) {
	exec, total := planStepExecution(t)

	over := newTokenApprove(t, []auth.Permission{cappedPerm(auth.ActionApproveAny, total-1)}, autoPlan(nil))
	requireApproveRefused(t, over.purchase, over.approve(exec), 403)

	atCap := newTokenApprove(t, []auth.Permission{cappedPerm(auth.ActionApproveAny, total)}, autoPlan(nil))
	requireApproveReached(t, atCap.purchase, atCap.approve(exec))
}

func TestApproveViaToken_PlanStep_ZeroTotalIs400(t *testing.T) {
	exec, _ := planStepExecution(t)
	exec.Recommendations[0].UpfrontCost = 0
	exec.Recommendations[0].MonthlyCost = nil
	f := newTokenApprove(t, []auth.Permission{cappedPerm(auth.ActionApproveOwn, 1000)}, autoPlan(nil))
	requireApproveRefused(t, f.purchase, f.approve(exec), 400)
}

func TestApproveViaToken_PlanStep_NoVerbHonorsPlanCap(t *testing.T) {
	exec, total := planStepExecution(t)
	under, over := total+1, total-1

	f := newTokenApprove(t, nil, autoPlan(&over))
	requireApproveRefused(t, f.purchase, f.approve(exec), 403)

	f = newTokenApprove(t, nil, autoPlan(&under))
	requireApproveReached(t, f.purchase, f.approve(exec))

	f = newTokenApprove(t, nil, autoPlan(&total))
	requireApproveReached(t, f.purchase, f.approve(exec))

	f = newTokenApprove(t, nil, autoPlan(nil))
	requireApproveRefused(t, f.purchase, f.approve(exec), 403)
}

func TestApproveViaToken_PlanStep_ManualPlan(t *testing.T) {
	exec, total := planStepExecution(t)
	over := total - 1

	noCap := newTokenApprove(t, nil, &config.PurchasePlan{ID: stepPlanID})
	requireApproveReached(t, noCap.purchase, noCap.approve(exec))

	withCap := newTokenApprove(t, nil, &config.PurchasePlan{ID: stepPlanID, MaxStepCommitment: &over})
	requireApproveRefused(t, withCap.purchase, withCap.approve(exec), 403)
}

func TestApproveConstraintsForSession_PlanStep_SessionlessAndAdminKey(t *testing.T) {
	ctx := context.Background()
	exec, total := planStepExecution(t)
	over := total - 1

	f := newTokenApprove(t, nil, autoPlan(&over))
	requireStatus(t, f.handler.approveConstraintsForSession(ctx, nil, exec), 403)

	under := total + 1
	f = newTokenApprove(t, nil, autoPlan(&under))
	require.NoError(t, f.handler.approveConstraintsForSession(ctx, nil, exec))

	f = newTokenApprove(t, nil, autoPlan(&over))
	require.NoError(t, f.handler.approveConstraintsForSession(ctx, &Session{UserID: apiKeyAdminUserID}, exec))
}

func TestApproveConstraintsForSession_UserCreatedRowUnchanged(t *testing.T) {
	ctx := context.Background()
	exec, total := planStepExecution(t)
	exec.PlanID = ""
	f := newTokenApprove(t, []auth.Permission{cappedPerm(auth.ActionApproveOwn, total-1)}, autoPlan(nil))
	// Another user's row with no plan: approve-own simply does not apply.
	other := "51515151-5151-5151-5151-515151515151"
	exec.CreatedByUserID = &other
	require.NoError(t, f.handler.approveConstraintsForSession(ctx, &Session{UserID: stepUser, Email: stepContact}, exec))
}

func TestApproveConstraintsForSession_PlanStep_PlanReadErrorFailsClosed(t *testing.T) {
	exec, _ := planStepExecution(t)
	f := newTokenApprove(t, nil, nil)
	f.handler.config.(*MockConfigStore).GetPurchasePlanFn = func(context.Context, string) (*config.PurchasePlan, error) {
		return nil, assert.AnError
	}
	err := f.handler.approveConstraintsForSession(context.Background(), nil, exec)
	require.ErrorIs(t, err, assert.AnError)
}
