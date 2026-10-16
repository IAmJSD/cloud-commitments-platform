package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #678: cancel-any / cancel-own Constraints must bound what a holder can
// cancel, on the session cancel, the revoke-token session branch and the
// planned-purchase DELETE. Answers come from the real matcher via
// grantPermissionsScoped; helpers are shared with the #404 revoke tests.

var cancelSess = &Session{UserID: revokeConstraintsUser}

func azureRec() config.RecommendationRecord {
	return recFor("acct-uuid", "azure", "compute", "westeurope")
}

func TestAuthorizeSessionCancel_ConstraintDimensions(t *testing.T) {
	t.Parallel()
	denying := map[string]*auth.PermissionConstraints{
		"providers":   {Providers: []string{"aws"}},
		"services":    {Services: []string{"rds"}},
		"regions":     {Regions: []string{"eastus"}},
		"account ids": {AccountIDs: []string{"other-acct"}},
	}
	allowing := map[string]*auth.PermissionConstraints{
		"providers":   {Providers: []string{"azure"}},
		"services":    {Services: []string{"compute"}},
		"regions":     {Regions: []string{"westeurope"}},
		"account ids": {AccountIDs: []string{"acct-uuid"}},
	}
	for _, verb := range []string{auth.ActionCancelAny, auth.ActionCancelOwn} {
		for dim, c := range denying {
			t.Run(verb+"/deny/"+dim, func(t *testing.T) {
				t.Parallel()
				h := newRevokeHandler(revokePerm(verb, c))
				err := h.authorizeSessionCancel(context.Background(), cancelSess, execWithRecs(revokeConstraintsUser, azureRec()))
				requireRevokeForbidden(t, err, constraintDenied)
			})
			t.Run(verb+"/allow/"+dim, func(t *testing.T) {
				t.Parallel()
				h := newRevokeHandler(revokePerm(verb, allowing[dim]))
				require.NoError(t, h.authorizeSessionCancel(context.Background(), cancelSess, execWithRecs(revokeConstraintsUser, azureRec())))
			})
		}
	}
}

func TestAuthorizeSessionCancel_StrictScopeAndRecommendations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	regions := revokePerm(auth.ActionCancelAny, &auth.PermissionConstraints{Regions: []string{"westeurope"}})
	inside, outside := azureRec(), recFor("acct-uuid", "azure", "compute", "eastus")

	t.Run("blank region denies a Regions grant", func(t *testing.T) {
		t.Parallel()
		err := newRevokeHandler(regions).authorizeSessionCancel(ctx, cancelSess, execWithRecs("", recFor("a", "azure", "compute", "")))
		requireRevokeForbidden(t, err, constraintDenied)
	})
	t.Run("unattributed account denies an AccountIDs grant", func(t *testing.T) {
		t.Parallel()
		h := newRevokeHandler(revokePerm(auth.ActionCancelAny, &auth.PermissionConstraints{AccountIDs: []string{"acct-uuid"}}))
		requireRevokeForbidden(t, h.authorizeSessionCancel(ctx, cancelSess, execWithRecs("", recFor("", "azure", "compute", "westeurope"))), constraintDenied)
	})
	t.Run("unconstrained grant allows blank dimensions", func(t *testing.T) {
		t.Parallel()
		h := newRevokeHandler(revokePerm(auth.ActionCancelAny, nil))
		require.NoError(t, h.authorizeSessionCancel(ctx, cancelSess, execWithRecs("", recFor("", "", "", ""))))
	})
	t.Run("every recommendation must be covered", func(t *testing.T) {
		t.Parallel()
		h := newRevokeHandler(regions)
		require.NoError(t, h.authorizeSessionCancel(ctx, cancelSess, execWithRecs("", inside, inside)))
		requireRevokeForbidden(t, h.authorizeSessionCancel(ctx, cancelSess, execWithRecs("", inside, outside)), constraintDenied)
	})
	t.Run("zero recommendations: constrained denies, unconstrained allows", func(t *testing.T) {
		t.Parallel()
		requireRevokeForbidden(t, newRevokeHandler(regions).authorizeSessionCancel(ctx, cancelSess, execWithRecs("")), constraintDenied)
		require.NoError(t, newRevokeHandler(revokePerm(auth.ActionCancelAny, nil)).authorizeSessionCancel(ctx, cancelSess, execWithRecs("")))
	})
}

func TestAuthorizeSessionCancel_ConstrainedAnyFallsBackToOwn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	constrained := &auth.PermissionConstraints{Providers: []string{"aws"}}
	any, own := revokePerm(auth.ActionCancelAny, constrained), revokePerm(auth.ActionCancelOwn, nil)

	t.Run("creator with unconstrained own is allowed", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, newRevokeHandler(any, own).authorizeSessionCancel(ctx, cancelSess, execWithRecs(revokeConstraintsUser, azureRec())))
	})
	t.Run("non-creator gets the constraint message, not the ownership one", func(t *testing.T) {
		t.Parallel()
		err := newRevokeHandler(any, own).authorizeSessionCancel(ctx, cancelSess, execWithRecs("someone-else", azureRec()))
		requireRevokeForbidden(t, err, constraintDenied)
	})
	t.Run("constrained any without own is denied", func(t *testing.T) {
		t.Parallel()
		requireRevokeForbidden(t, newRevokeHandler(any).authorizeSessionCancel(ctx, cancelSess, execWithRecs(revokeConstraintsUser, azureRec())), constraintDenied)
	})
	t.Run("both constrained and outside is denied", func(t *testing.T) {
		t.Parallel()
		h := newRevokeHandler(any, revokePerm(auth.ActionCancelOwn, constrained))
		requireRevokeForbidden(t, h.authorizeSessionCancel(ctx, cancelSess, execWithRecs(revokeConstraintsUser, azureRec())), constraintDenied)
	})
	t.Run("plain ownership and missing-verb denials are unchanged", func(t *testing.T) {
		t.Parallel()
		requireRevokeForbidden(t, newRevokeHandler(own).authorizeSessionCancel(ctx, cancelSess, execWithRecs("someone-else", azureRec())), "cannot cancel another user's pending purchase")
		requireRevokeForbidden(t, newRevokeHandler().authorizeSessionCancel(ctx, cancelSess, execWithRecs("", azureRec())), "requires cancel-any or cancel-own")
	})
}

func TestAuthorizeSessionCancel_ConstraintStoreErrorDoesNotFallBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	boom := errors.New("constraint store down")
	mockAuth := new(MockAuthService)
	mockAuth.On("HasPermissionAPI", ctx, revokeConstraintsUser, auth.ActionCancelAny, auth.ResourcePurchases).Return(true, nil)
	mockAuth.On("HasPermissionForConstraintsAPI", ctx, revokeConstraintsUser, auth.ActionCancelAny, auth.ResourcePurchases, mock.Anything).Return(false, boom)
	// cancel-own is deliberately unregistered: querying it fails the test.

	h := &Handler{auth: mockAuth, config: &MockConfigStore{}}
	err := h.authorizeSessionCancel(ctx, cancelSess, execWithRecs(revokeConstraintsUser, azureRec()))
	require.ErrorIs(t, err, boom)
	_, isClient := IsClientError(err)
	assert.False(t, isClient)
}

func TestAuthorizeSessionCancel_AdminAndAPIKeys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exec := execWithRecs("someone-else", azureRec())

	t.Run("stateless admin API key passes", func(t *testing.T) {
		t.Parallel()
		h := newRevokeHandler(revokePerm(auth.ActionCancelAny, &auth.PermissionConstraints{Providers: []string{"aws"}}))
		require.NoError(t, h.authorizeSessionCancel(ctx, &Session{UserID: apiKeyAdminUserID}, exec))
	})
	t.Run("admin wildcard carrying Constraints is enforced", func(t *testing.T) {
		t.Parallel()
		h := newRevokeHandler(auth.Permission{Action: auth.ActionAdmin, Resource: auth.ResourceAll, Constraints: &auth.PermissionConstraints{Providers: []string{"aws"}}})
		requireRevokeForbidden(t, h.authorizeSessionCancel(ctx, cancelSess, exec), constraintDenied)
	})
	t.Run("unconstrained admin wildcard passes", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, newRevokeHandler(auth.Permission{Action: auth.ActionAdmin, Resource: auth.ResourceAll}).authorizeSessionCancel(ctx, cancelSess, exec))
	})
	t.Run("user API key sessions use the key's constraints", func(t *testing.T) {
		t.Parallel()
		for _, allowed := range []bool{false, true} {
			mockAuth := new(MockAuthService)
			mockAuth.On("HasPermissionAPI", ctx, revokeConstraintsUser, auth.ActionCancelAny, auth.ResourcePurchases).Return(true, nil)
			mockAuth.On("HasPermissionAPI", ctx, revokeConstraintsUser, auth.ActionCancelOwn, auth.ResourcePurchases).Return(false, nil).Maybe()
			mockAuth.On("HasAPIKeyPermissionForConstraintsAPI", ctx, "key-1", revokeConstraintsUser, auth.ActionCancelAny, auth.ResourcePurchases, mock.Anything).Return(allowed, nil).Once()
			h := &Handler{auth: mockAuth, config: &MockConfigStore{}}
			err := h.authorizeSessionCancel(ctx, &Session{UserID: revokeConstraintsUser, UserAPIKeyID: "key-1"}, exec)
			if allowed {
				require.NoError(t, err)
			} else {
				requireRevokeForbidden(t, err, constraintDenied)
			}
			mockAuth.AssertExpectations(t)
		}
	})
}

// cancelRouteExec is a pending azure execution with a contact approver, so the
// token branch is reachable when a test asks for it.
func cancelRouteExec(status string) *config.PurchaseExecution {
	acct := "acct-1"
	future := time.Now().Add(time.Hour)
	return &config.PurchaseExecution{
		ExecutionID: cancelExecID, Status: status, CloudAccountID: &acct,
		ApprovalToken: config.HashApprovalToken(tokenErrRawToken), ApprovalTokenExpiresAt: &future,
		Recommendations: []config.RecommendationRecord{{ID: "r1", Provider: "azure", Service: "compute", Region: "westeurope", CloudAccountID: &acct}},
	}
}

// cancelRouteHandler answers authorization from the real matcher for a caller
// whose session email is the execution's contact approver.
func cancelRouteHandler(exec *config.PurchaseExecution, perms ...auth.Permission) (*Handler, *MockConfigStore) {
	store := new(MockConfigStore)
	store.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
		return &config.CloudAccount{ID: id, Name: id, ContactEmail: "contact@example.com"}, nil
	}
	store.On("GetExecutionByID", mock.Anything, cancelExecID).Return(exec, nil)
	store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{}, nil).Maybe()
	store.On("CancelExecutionAtomic", mock.Anything, mock.Anything, cancelExecID, mock.Anything).Return(true, "canceled", nil).Maybe()
	store.On("DeleteSuppressionsByExecutionTx", mock.Anything, mock.Anything, cancelExecID).Return(nil).Maybe()
	store.On("TransitionExecutionStatus", mock.Anything, cancelExecID, mock.Anything, mock.Anything, mock.Anything).
		Return(&config.PurchaseExecution{ExecutionID: cancelExecID, Status: "canceled"}, nil).Maybe()
	store.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(nil).Maybe()
	store.On("SetCancelledBy", mock.Anything, cancelExecID, mock.Anything).Return(nil).Maybe()

	m := new(MockAuthService)
	m.On("ValidateSession", mock.Anything, "sess-tok").Return(&Session{UserID: revokeConstraintsUser, Email: "contact@example.com"}, nil)
	m.On("ValidateCSRFToken", mock.Anything, "sess-tok", "").Return(nil).Maybe()
	m.grantPermissions(perms)
	return &Handler{config: store, auth: m, purchase: purchase.NewManager(purchase.ManagerConfig{ConfigStore: store})}, store
}

func awsOnly(verb string) auth.Permission {
	return revokePerm(verb, &auth.PermissionConstraints{Providers: []string{"aws"}})
}

func TestCancelPurchase_ConstrainedCancelAnyIsDeniedWithAndWithoutToken(t *testing.T) {
	t.Parallel()
	for _, token := range []string{"", tokenErrRawToken} {
		t.Run("token="+token, func(t *testing.T) {
			t.Parallel()
			h, store := cancelRouteHandler(cancelRouteExec("pending"), awsOnly(auth.ActionCancelAny))
			_, err := h.cancelPurchase(context.Background(), sessionCancelReq(), cancelExecID, token)
			requireRevokeForbidden(t, err, constraintDenied)
			store.AssertNotCalled(t, "CancelExecutionAtomic", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

func TestCancelPurchase_ContactApproverWithoutCancelVerbStillUsesToken(t *testing.T) {
	t.Parallel()
	h, store := cancelRouteHandler(cancelRouteExec("pending"))
	res, err := h.cancelPurchase(context.Background(), sessionCancelReq(), cancelExecID, tokenErrRawToken)
	require.NoError(t, err)
	assert.Equal(t, "canceled", res.(map[string]string)["status"])
	store.AssertCalled(t, "CancelExecutionAtomic", mock.Anything, mock.Anything, cancelExecID, mock.Anything)
}

func TestCancelPurchase_InScopeCancelAnyStillCancels(t *testing.T) {
	t.Parallel()
	h, _ := cancelRouteHandler(cancelRouteExec("pending"), revokePerm(auth.ActionCancelAny, &auth.PermissionConstraints{Providers: []string{"azure"}}))
	res, err := h.cancelPurchase(context.Background(), sessionCancelReq(), cancelExecID, "")
	require.NoError(t, err)
	assert.Equal(t, "canceled", res.(map[string]string)["status"])
}

func revokeTokenReq(token string) *events.LambdaFunctionURLRequest {
	req := tokenErrRequest("revoke", token)
	req.RequestContext.HTTP.Path = "/api/purchases/revoke/" + cancelExecID
	return req
}

func TestRevokeViaEmailToken_SessionBranchEnforcesCancelConstraints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("constrained out is denied and nothing is written", func(t *testing.T) {
		t.Parallel()
		h, store := cancelRouteHandler(cancelRouteExec("completed"), awsOnly(auth.ActionCancelAny))
		_, err := h.revokeViaEmailToken(ctx, revokeTokenReq(""), cancelExecID, "")
		requireRevokeForbidden(t, err, constraintDenied)
		store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})
	t.Run("constrained out is terminal even with a token", func(t *testing.T) {
		t.Parallel()
		h, store := cancelRouteHandler(cancelRouteExec("completed"), awsOnly(auth.ActionCancelAny))
		_, err := h.revokeViaEmailToken(ctx, revokeTokenReq(tokenErrRawToken), cancelExecID, tokenErrRawToken)
		requireRevokeForbidden(t, err, constraintDenied)
		store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})
	t.Run("constrained in proceeds", func(t *testing.T) {
		t.Parallel()
		h, store := cancelRouteHandler(cancelRouteExec("completed"), revokePerm(auth.ActionCancelAny, &auth.PermissionConstraints{Providers: []string{"azure"}}))
		_, err := h.revokeViaEmailToken(ctx, revokeTokenReq(""), cancelExecID, "")
		require.NoError(t, err)
		store.AssertCalled(t, "TransitionExecutionStatus", mock.Anything, cancelExecID, mock.Anything, "revocation_requested", mock.Anything)
	})
}
