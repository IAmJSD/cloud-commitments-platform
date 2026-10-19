package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// Issue #92 regression guards for the per-execution account-scope gate on the
// execution endpoints. The caller is an admin restricted by allowed_accounts
// who holds every money verb, so every RBAC check passes and only the scope
// gate can stop the action.
const (
	scopeTestExecID = "92929292-0000-0000-0000-000000000001"
	scopeTestPlanID = "92929292-0000-0000-0000-000000000002"
)

var errScopeTestReached = errors.New("mutation reached")

var (
	scopeTestProd  = config.CloudAccount{ID: "acc-prod", Name: "Production", ContactEmail: "prod-owner@example.com"}
	scopeTestStage = config.CloudAccount{ID: "acc-stage", Name: "Staging", ContactEmail: "operator@example.com"}
)

// scopeTestKind is one shape of execution, differing in how its buy-in
// accounts are derived (executionAccounts).
type scopeTestKind struct {
	name           string
	planID         string
	cloudAccountID *string
	recs           []config.RecommendationRecord
	// partialScope holds some but not all of the accounts the execution can
	// buy in (or, for the single-account child, the plan's other account).
	partialScope []string
}

// scopeTestRec is a purchasable recommendation, so an allowed call clears
// the post-scope constraint checks and reaches the mutation.
func scopeTestRec(id string, account *string) config.RecommendationRecord {
	return config.RecommendationRecord{
		ID: id, Provider: "aws", Service: "rds", Region: "us-east-1",
		Count: 1, Term: 1, Payment: "all-upfront", UpfrontCost: 100, CloudAccountID: account,
	}
}

var scopeTestBothRecs = []config.RecommendationRecord{
	scopeTestRec("rec-prod", &scopeTestProd.ID),
	scopeTestRec("rec-stage", &scopeTestStage.ID),
}

var scopeTestKinds = []scopeTestKind{
	// Plan-level parent: CloudAccountID nil, fans out to every plan account.
	{name: "plan parent", planID: scopeTestPlanID, recs: scopeTestBothRecs, partialScope: []string{scopeTestProd.ID}},
	// Fan-out child of the same plan, buying only in Staging.
	{name: "fan-out child", planID: scopeTestPlanID, cloudAccountID: &scopeTestStage.ID,
		recs: []config.RecommendationRecord{scopeTestRec("rec-stage", &scopeTestStage.ID)}, partialScope: []string{scopeTestProd.ID}},
	// Ad-hoc web purchase: plan_id NULL, one recommendation per account.
	{name: "ad-hoc", recs: scopeTestBothRecs, partialScope: []string{scopeTestProd.ID}},
}

type scopeTestScope struct {
	name  string
	scope func(k scopeTestKind) []string
	allow bool
}

var scopeTestScopes = []scopeTestScope{
	{name: "all in scope", allow: true, scope: func(scopeTestKind) []string { return []string{scopeTestProd.ID, scopeTestStage.ID} }},
	{name: "partial scope", scope: func(k scopeTestKind) []string { return k.partialScope }},
	{name: "no scope", scope: func(scopeTestKind) []string { return []string{"Other"} }},
}

type scopeTestEndpoint struct {
	name   string
	status string
	call   func(h *Handler, req *events.LambdaFunctionURLRequest) (any, error)
	// readOnly endpoints reach no mutation, so an allowed call succeeds
	// instead of answering errScopeTestReached.
	readOnly bool
}

var scopeTestEndpoints = []scopeTestEndpoint{
	{
		name: "approve", status: "pending",
		call: func(h *Handler, req *events.LambdaFunctionURLRequest) (any, error) {
			return h.approvePurchase(context.Background(), req, scopeTestExecID, "")
		},
	},
	{
		name: "cancel", status: "pending",
		call: func(h *Handler, req *events.LambdaFunctionURLRequest) (any, error) {
			return h.cancelPurchase(context.Background(), req, scopeTestExecID, "")
		},
	},
	{
		name: "retry", status: "failed",
		call: func(h *Handler, req *events.LambdaFunctionURLRequest) (any, error) {
			return h.retryPurchase(context.Background(), req, scopeTestExecID)
		},
	},
	{
		name: "revoke completed via session", status: "completed",
		call: func(h *Handler, req *events.LambdaFunctionURLRequest) (any, error) {
			return h.revokeViaEmailToken(context.Background(), req, scopeTestExecID, "")
		},
	},
	{
		name: "revoke scheduled", status: "scheduled",
		call: func(h *Handler, req *events.LambdaFunctionURLRequest) (any, error) {
			return h.revokePurchase(context.Background(), req, scopeTestExecID)
		},
	},
	{
		name: "run now", status: "pending",
		call: func(h *Handler, req *events.LambdaFunctionURLRequest) (any, error) {
			return h.runPlannedPurchase(context.Background(), req, scopeTestExecID)
		},
	},
	{
		name: "pause", status: "pending",
		call: func(h *Handler, req *events.LambdaFunctionURLRequest) (any, error) {
			return h.pausePlannedPurchase(context.Background(), req, scopeTestExecID)
		},
	},
	{
		name: "resume", status: "paused",
		call: func(h *Handler, req *events.LambdaFunctionURLRequest) (any, error) {
			return h.resumePlannedPurchase(context.Background(), req, scopeTestExecID)
		},
	},
	{
		name: "delete", status: "pending",
		call: func(h *Handler, req *events.LambdaFunctionURLRequest) (any, error) {
			return h.deletePlannedPurchase(context.Background(), req, scopeTestExecID)
		},
	},
	{
		name: "details", status: "completed", readOnly: true,
		call: func(h *Handler, req *events.LambdaFunctionURLRequest) (any, error) {
			return h.getPurchaseDetails(context.Background(), req, scopeTestExecID)
		},
	},
}

// scopeTestExecution builds the execution row for a kind.
func scopeTestExecution(k scopeTestKind, status string) *config.PurchaseExecution {
	creator := "operator-1"
	return &config.PurchaseExecution{
		ExecutionID:     scopeTestExecID,
		PlanID:          k.planID,
		Status:          status,
		CloudAccountID:  k.cloudAccountID,
		Recommendations: k.recs,
		CreatedByUserID: &creator,
		ScheduledDate:   time.Now().Add(time.Hour), // not due, so resume needs no reschedule
	}
}

// newScopeTestHandler wires a handler whose session is scoped to scope and
// whose store serves exec. GetPlanAccounts("") errors the way the UUID
// plan_id column does in Postgres, so a gate that queries it for an ad-hoc
// execution surfaces as a 500 rather than a quiet deny.
//
// The caller constructs the mocks so their types resolve at its assertion
// sites for TestNoUnfailableMockAssertions.
func newScopeTestHandler(t *testing.T, exec *config.PurchaseExecution, scope []string, store *MockConfigStore, mockPurchase *MockPurchaseManager) *Handler {
	t.Helper()
	mockAuth := new(MockAuthService)

	mockAuth.On("ValidateSession", mock.Anything, "operator-token").Return(&Session{
		UserID: "operator-1", Email: "operator@example.com",
	}, nil)
	mockAuth.On("ValidateCSRFToken", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	// admin:* plus the money verbs it deliberately does not cover
	// (adminCarvedOuts), so every RBAC gate passes.
	mockAuth.grantPermissionsScoped([]auth.Permission{
		{Action: auth.ActionAdmin, Resource: auth.ResourceAll},
		{Action: auth.ActionExecute, Resource: auth.ResourcePurchases},
		{Action: auth.ActionApproveAny, Resource: auth.ResourcePurchases},
		{Action: auth.ActionRetryAny, Resource: auth.ResourcePurchases},
	}, scope)

	store.On("GetExecutionByID", mock.Anything, scopeTestExecID).Return(exec, nil)
	store.GetPlanAccountsFn = func(_ context.Context, planID string) ([]config.CloudAccount, error) {
		switch planID {
		case "":
			return nil, errors.New(`invalid input syntax for type uuid: ""`)
		case scopeTestPlanID:
			return []config.CloudAccount{scopeTestProd, scopeTestStage}, nil
		}
		return nil, nil
	}
	// The registered accounts, which getAccountScope loads to resolve
	// display names to account IDs (#433, #1034).
	store.ListCloudAccountsFn = func(context.Context, config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return []config.CloudAccount{scopeTestProd, scopeTestStage}, nil
	}
	store.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
		for _, a := range []config.CloudAccount{scopeTestProd, scopeTestStage} {
			if a.ID == id {
				return &a, nil
			}
		}
		return nil, nil
	}
	// Every mutation answers errScopeTestReached, so an allowed call proves
	// it got past all gates without wiring each endpoint's success path.
	store.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(errScopeTestReached).Maybe()
	store.On("TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil, errScopeTestReached).Maybe()
	store.On("CancelExecutionAtomic", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(false, "", errScopeTestReached).Maybe()
	store.On("CancelScheduledExecutionAtomic", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(false, "", errScopeTestReached).Maybe()
	mockPurchase.On("ApproveAndExecute", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil, "", errScopeTestReached).Maybe()
	mockPurchase.On("RunPlannedPurchaseNow", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil, "", errScopeTestReached).Maybe()
	mockPurchase.On("CancelExecution", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	return &Handler{auth: mockAuth, config: store, purchase: mockPurchase}
}

func scopeTestRequest() *events.LambdaFunctionURLRequest {
	return &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"Authorization": "Bearer operator-token", "X-CSRF-Token": "csrf"},
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "POST"},
		},
	}
}

// TestExecutionEndpoints_ScopeMatrix requires EVERY account an execution can
// buy in to be in scope: a session holding only some of them (or none) gets
// the enumeration-safe 404 and never reaches the mutation, while a session
// holding all of them passes the gate.
func TestExecutionEndpoints_ScopeMatrix(t *testing.T) {
	for _, k := range scopeTestKinds {
		for _, sc := range scopeTestScopes {
			for _, ep := range scopeTestEndpoints {
				t.Run(k.name+"/"+sc.name+"/"+ep.name, func(t *testing.T) {
					store := new(MockConfigStore)
					mockPurchase := new(MockPurchaseManager)
					h := newScopeTestHandler(t, scopeTestExecution(k, ep.status), sc.scope(k), store, mockPurchase)

					_, err := ep.call(h, scopeTestRequest())

					if !sc.allow {
						require.Error(t, err)
						assert.True(t, isNotFound404(err), "expected 404 not-found, got %v", err)
						// A denied call reaches no mutation of any endpoint.
						store.AssertNotCalled(t, "SavePurchaseExecution")
						store.AssertNotCalled(t, "TransitionExecutionStatus")
						store.AssertNotCalled(t, "CancelExecutionAtomic")
						store.AssertNotCalled(t, "CancelScheduledExecutionAtomic")
						mockPurchase.AssertNotCalled(t, "ApproveAndExecute", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
						mockPurchase.AssertNotCalled(t, "RunPlannedPurchaseNow", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
						mockPurchase.AssertNotCalled(t, "CancelExecution", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
						return
					}
					if ep.readOnly {
						require.NoError(t, err)
						return
					}
					assert.ErrorContains(t, err, errScopeTestReached.Error())
				})
			}
		}
	}
}

// TestExecutionEndpoints_OutOfScopeNoTokenIs404 guards the token fall-through:
// without a token an out-of-scope session must get the same 404 as a missing
// row, not the token/session branch's status guards (409/410/401), which
// would confirm the execution exists and leak its status.
func TestExecutionEndpoints_OutOfScopeNoTokenIs404(t *testing.T) {
	adhoc := scopeTestKinds[2]
	for _, tc := range []struct {
		name   string
		status string
		call   func(h *Handler, req *events.LambdaFunctionURLRequest) (any, error)
	}{
		{"approve wrong status", "completed", func(h *Handler, req *events.LambdaFunctionURLRequest) (any, error) {
			return h.approvePurchase(context.Background(), req, scopeTestExecID, "")
		}},
		{"cancel wrong status", "completed", func(h *Handler, req *events.LambdaFunctionURLRequest) (any, error) {
			return h.cancelPurchase(context.Background(), req, scopeTestExecID, "")
		}},
		{"revoke completed", "completed", func(h *Handler, req *events.LambdaFunctionURLRequest) (any, error) {
			return h.revokeViaEmailToken(context.Background(), req, scopeTestExecID, "")
		}},
		{"revoke pending", "pending", func(h *Handler, req *events.LambdaFunctionURLRequest) (any, error) {
			return h.revokeViaEmailToken(context.Background(), req, scopeTestExecID, "")
		}},
		{"revoke notified", "notified", func(h *Handler, req *events.LambdaFunctionURLRequest) (any, error) {
			return h.revokeViaEmailToken(context.Background(), req, scopeTestExecID, "")
		}},
		{"revoke failed", "failed", func(h *Handler, req *events.LambdaFunctionURLRequest) (any, error) {
			return h.revokeViaEmailToken(context.Background(), req, scopeTestExecID, "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newScopeTestHandler(t, scopeTestExecution(adhoc, tc.status), []string{"Other"}, new(MockConfigStore), new(MockPurchaseManager))
			_, err := tc.call(h, scopeTestRequest())
			assert.True(t, isNotFound404(err), "expected 404 not-found, got %v", err)
		})
	}
}

// isNotFound404 accepts either form of the enumeration-safe 404: errNotFound
// from the scope gate, or the "execution not found" ClientError the approve,
// cancel and revoke routes answer for both a missing and a hidden execution
// (issue #597).
func isNotFound404(err error) bool {
	if IsNotFoundError(err) {
		return true
	}
	ce, ok := IsClientError(err)
	return ok && ce.code == 404 && ce.message == "execution not found"
}

// TestRevoke_AuthorizedNonRevocableStatusIs409: moving the status check
// behind authorization must not drop it. An authorized caller, via the
// session or the email token, still gets the 409 for a status with nothing
// to revoke, and no mutation runs.
func TestRevoke_AuthorizedNonRevocableStatusIs409(t *testing.T) {
	stageOnly := scopeTestKind{recs: []config.RecommendationRecord{scopeTestRec("rec-stage", &scopeTestStage.ID)}}
	for _, tc := range []struct {
		name  string
		scope []string
		token string
	}{
		{"session", []string{scopeTestStage.ID}, ""},
		{"token", []string{"Other"}, "email-token"},
	} {
		for _, status := range []string{"pending", "notified", "failed"} {
			t.Run(tc.name+"/"+status, func(t *testing.T) {
				exec := scopeTestExecution(stageOnly, status)
				exec.ApprovalToken = config.HashApprovalToken("email-token")
				store := new(MockConfigStore)
				h := newScopeTestHandler(t, exec, tc.scope, store, new(MockPurchaseManager))

				_, err := h.revokeViaEmailToken(context.Background(), scopeTestRequest(), scopeTestExecID, tc.token)

				var ce *clientError
				require.ErrorAs(t, err, &ce)
				assert.Equal(t, 409, ce.code, "got %v", err)
				if status == "pending" {
					assert.Contains(t, ce.message, "Cancel")
				}
				store.AssertNotCalled(t, "TransitionExecutionStatus")
			})
		}
	}
}

// TestCancel_OutOfScopeSessionWithTokenUsesContactGate is the positive
// control for the fall-through: a session outside the account scope that
// carries the email token reaches the token branch, and succeeds because the
// session email is the account's contact_email.
func TestCancel_OutOfScopeSessionWithTokenUsesContactGate(t *testing.T) {
	exec := scopeTestExecution(scopeTestKind{recs: []config.RecommendationRecord{
		scopeTestRec("rec-stage", &scopeTestStage.ID),
	}}, "pending")
	mockPurchase := new(MockPurchaseManager)
	h := newScopeTestHandler(t, exec, []string{scopeTestProd.ID}, new(MockConfigStore), mockPurchase)

	res, err := h.cancelPurchase(context.Background(), scopeTestRequest(), scopeTestExecID, "email-token")

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"status": "canceled"}, res)
	mockPurchase.AssertCalled(t, "CancelExecution", mock.Anything, scopeTestExecID, "email-token", "operator@example.com")
}

// TestApprove_OutOfScopeSessionWithTokenUsesContactGate is the approve
// counterpart of the cancel control above: an out-of-scope session carrying
// the email token falls through to the token branch and is approved because
// the session email is the account's contact_email. The negative case (a
// session that is not the contact email) is covered at
// handler_purchases_test.go:144.
func TestApprove_OutOfScopeSessionWithTokenUsesContactGate(t *testing.T) {
	exec := scopeTestExecution(scopeTestKind{recs: []config.RecommendationRecord{
		scopeTestRec("rec-stage", &scopeTestStage.ID),
	}}, "pending")
	store := new(MockConfigStore)
	mockPurchase := new(MockPurchaseManager)
	h := newScopeTestHandler(t, exec, []string{scopeTestProd.ID}, store, mockPurchase)
	mockPurchase.On("ApproveExecution", mock.Anything, scopeTestExecID, "email-token", "operator@example.com").Return("revocation-token", nil)
	store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{}, nil)

	res, err := h.approvePurchase(context.Background(), scopeTestRequest(), scopeTestExecID, "email-token")

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"status": "completed"}, res)
	mockPurchase.AssertNotCalled(t, "ApproveAndExecute", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	mockPurchase.AssertCalled(t, "ApproveExecution", mock.Anything, scopeTestExecID, "email-token", "operator@example.com")
}

// TestExecutionAccounts_AdHocUnattributedDenied: an ad-hoc execution with a
// recommendation lacking a cloud account cannot be attributed, so a scoped
// session is denied even when every attributed account is in scope.
func TestExecutionAccounts_AdHocUnattributedDenied(t *testing.T) {
	empty := ""
	for _, missing := range []*string{nil, &empty} {
		exec := scopeTestExecution(scopeTestKind{recs: []config.RecommendationRecord{
			scopeTestRec("rec-prod", &scopeTestProd.ID),
			scopeTestRec("rec-none", missing),
		}}, "pending")
		h := newScopeTestHandler(t, exec, []string{scopeTestProd.ID, scopeTestStage.ID}, new(MockConfigStore), new(MockPurchaseManager))
		err := h.requireExecutionAccess(context.Background(), &Session{UserID: "operator-1"}, scopeTestExecID)
		assert.True(t, IsNotFoundError(err), "expected 404 not-found, got %v", err)
	}
}

// TestPlannedPurchases_HidesPartiallyScopedParent: the list filters with the
// same rule as the per-row gate, so a plan parent that also buys in an
// out-of-scope account is hidden from a partially-scoped session.
func TestPlannedPurchases_HidesPartiallyScopedParent(t *testing.T) {
	for _, tc := range []struct {
		scope []string
		want  int
	}{
		{[]string{scopeTestProd.ID}, 0},
		{[]string{scopeTestProd.ID, scopeTestStage.ID}, 1},
	} {
		exec := scopeTestExecution(scopeTestKinds[0], "pending")
		store := new(MockConfigStore)
		h := newScopeTestHandler(t, exec, tc.scope, store, new(MockPurchaseManager))
		store.On("GetPlannedExecutions", mock.Anything, mock.Anything, mock.Anything).Return([]config.PurchaseExecution{*exec}, nil)
		store.On("ListPurchasePlans", mock.Anything, mock.Anything).Return([]config.PurchasePlan{{ID: scopeTestPlanID, Name: "p"}}, nil)

		resp, err := h.getPlannedPurchases(context.Background(), scopeTestRequest())
		require.NoError(t, err)
		assert.Len(t, resp.Purchases, tc.want, "scope %v", tc.scope)
	}
}
