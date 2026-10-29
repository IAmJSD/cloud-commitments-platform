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
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
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
			// The verbs are probed on the key, not the owner (issue #1005).
			probe := []auth.PermissionConstraints{{}}
			mockAuth.On("HasAPIKeyPermissionForConstraintsAPI", ctx, "key-1", revokeConstraintsUser, auth.ActionCancelAny, auth.ResourcePurchases, probe).Return(true, nil)
			mockAuth.On("HasAPIKeyPermissionForConstraintsAPI", ctx, "key-1", revokeConstraintsUser, auth.ActionCancelOwn, auth.ResourcePurchases, probe).Return(false, nil).Maybe()
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
func cancelRouteHandler(t *testing.T, exec *config.PurchaseExecution, perms ...auth.Permission) (*Handler, *MockConfigStore) {
	t.Helper()
	store := new(MockConfigStore)
	t.Cleanup(func() { store.AssertExpectations(t) })
	store.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
		return &config.CloudAccount{ID: id, Name: id, ContactEmail: "contact@example.com"}, nil
	}
	store.On("GetExecutionByID", mock.Anything, cancelExecID).Return(exec, nil)
	store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{}, nil).Maybe()
	store.On("CancelExecutionAtomic", mock.Anything, mock.Anything, cancelExecID, mock.Anything).Return(true, "canceled", nil).Maybe()
	store.On("CancelExecutionAtomicTokenPinned", mock.Anything, mock.Anything, cancelExecID, mock.Anything, mock.Anything).Return(true, "canceled", nil).Maybe()
	store.On("DeleteSuppressionsByExecutionTx", mock.Anything, mock.Anything, cancelExecID).Return(nil).Maybe()
	store.On("TransitionExecutionStatus", mock.Anything, cancelExecID, mock.Anything, mock.Anything, mock.Anything).
		Return(&config.PurchaseExecution{ExecutionID: cancelExecID, Status: "canceled"}, nil).Maybe()
	store.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(nil).Maybe()
	store.On("SetCancelledBy", mock.Anything, cancelExecID, mock.Anything).Return(nil).Maybe()

	m := new(MockAuthService)
	t.Cleanup(func() { m.AssertExpectations(t) })
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
			h, store := cancelRouteHandler(t, cancelRouteExec("pending"), awsOnly(auth.ActionCancelAny))
			_, err := h.cancelPurchase(context.Background(), sessionCancelReq(), cancelExecID, token)
			requireRevokeForbidden(t, err, constraintDenied)
			store.AssertNotCalled(t, "CancelExecutionAtomic", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			store.AssertNotCalled(t, "CancelExecutionAtomicTokenPinned", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

func TestCancelPurchase_ContactApproverWithoutCancelVerbStillUsesToken(t *testing.T) {
	t.Parallel()
	h, store := cancelRouteHandler(t, cancelRouteExec("pending"))
	res, err := h.cancelPurchase(context.Background(), sessionCancelReq(), cancelExecID, tokenErrRawToken)
	require.NoError(t, err)
	assert.Equal(t, "canceled", res.(map[string]string)["status"])
	store.AssertCalled(t, "CancelExecutionAtomicTokenPinned", mock.Anything, mock.Anything, cancelExecID, mock.Anything, tokenErrRawToken)
}

func TestCancelPurchase_InScopeCancelAnyStillCancels(t *testing.T) {
	t.Parallel()
	h, _ := cancelRouteHandler(t, cancelRouteExec("pending"), revokePerm(auth.ActionCancelAny, &auth.PermissionConstraints{Providers: []string{"azure"}}))
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
		h, store := cancelRouteHandler(t, cancelRouteExec("completed"), awsOnly(auth.ActionCancelAny))
		_, err := h.revokeViaEmailToken(ctx, revokeTokenReq(""), cancelExecID, "")
		requireRevokeForbidden(t, err, constraintDenied)
		store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})
	t.Run("constrained out is terminal even with a token", func(t *testing.T) {
		t.Parallel()
		h, store := cancelRouteHandler(t, cancelRouteExec("completed"), awsOnly(auth.ActionCancelAny))
		_, err := h.revokeViaEmailToken(ctx, revokeTokenReq(tokenErrRawToken), cancelExecID, tokenErrRawToken)
		requireRevokeForbidden(t, err, constraintDenied)
		store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})
	t.Run("constrained in proceeds", func(t *testing.T) {
		t.Parallel()
		h, store := cancelRouteHandler(t, cancelRouteExec("completed"), revokePerm(auth.ActionCancelAny, &auth.PermissionConstraints{Providers: []string{"azure"}}))
		_, err := h.revokeViaEmailToken(ctx, revokeTokenReq(""), cancelExecID, "")
		require.NoError(t, err)
		store.AssertCalled(t, "TransitionExecutionStatus", mock.Anything, cancelExecID, mock.Anything, "revocation_requested", mock.Anything)
	})
}

func deletePlannedReq() *events.LambdaFunctionURLRequest {
	return &events.LambdaFunctionURLRequest{Headers: map[string]string{"authorization": "Bearer sess-tok"}}
}

func TestDeletePlannedPurchase_CancelConstraints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	creator := revokeConstraintsUser
	del := auth.Permission{Action: auth.ActionDelete, Resource: auth.ResourcePurchases}
	run := func(creatorID *string, perms ...auth.Permission) (*DeletePlannedPurchaseResponse, *MockConfigStore, error) {
		exec := cancelRouteExec("pending")
		exec.CreatedByUserID = creatorID
		h, store := cancelRouteHandler(t, exec, perms...)
		resp, err := h.deletePlannedPurchase(ctx, deletePlannedReq(), cancelExecID)
		return resp, store, err
	}
	notTransitioned := func(t *testing.T, store *MockConfigStore) {
		t.Helper()
		store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	}

	t.Run("constrained cancel-any out of scope is denied", func(t *testing.T) {
		t.Parallel()
		_, store, err := run(nil, awsOnly(auth.ActionCancelAny))
		requireRevokeForbidden(t, err, constraintDenied)
		notTransitioned(t, store)
	})
	t.Run("unconstrained and in-scope cancel-any cancel", func(t *testing.T) {
		t.Parallel()
		for _, c := range []*auth.PermissionConstraints{nil, {Providers: []string{"azure"}}} {
			resp, _, err := run(nil, revokePerm(auth.ActionCancelAny, c))
			require.NoError(t, err)
			assert.Equal(t, "canceled", resp.Status)
		}
	})
	t.Run("constrained any falls back to creator with unconstrained own", func(t *testing.T) {
		t.Parallel()
		resp, _, err := run(&creator, awsOnly(auth.ActionCancelAny), revokePerm(auth.ActionCancelOwn, nil))
		require.NoError(t, err)
		assert.Equal(t, "canceled", resp.Status)
	})
	t.Run("constrained any for a non-creator keeps the constraint message", func(t *testing.T) {
		t.Parallel()
		other := "someone-else"
		_, store, err := run(&other, awsOnly(auth.ActionCancelAny), revokePerm(auth.ActionCancelOwn, nil))
		requireRevokeForbidden(t, err, constraintDenied)
		notTransitioned(t, store)
	})
	t.Run("creator with constrained cancel-own out of scope is denied", func(t *testing.T) {
		t.Parallel()
		_, store, err := run(&creator, awsOnly(auth.ActionCancelOwn))
		requireRevokeForbidden(t, err, constraintDenied)
		notTransitioned(t, store)
	})
	t.Run("creator with delete and no cancel-own still deletes", func(t *testing.T) {
		t.Parallel()
		resp, _, err := run(&creator, del)
		require.NoError(t, err)
		assert.Equal(t, "canceled", resp.Status)
	})
	t.Run("row with no creator is denied for delete-only callers", func(t *testing.T) {
		t.Parallel()
		_, store, err := run(nil, del)
		requireRevokeForbidden(t, err, "another user's scheduled purchase")
		notTransitioned(t, store)
	})
	t.Run("update-any out of scope is denied (#961)", func(t *testing.T) {
		t.Parallel()
		_, store, err := run(nil, awsOnly(auth.ActionUpdateAny))
		requireRevokeForbidden(t, err, constraintDenied)
		notTransitioned(t, store)
	})
}

// plannedKeyHandler models a delete:purchases user-API-key request whose owner
// created the execution and holds cancel-own. keyCancelOwn is what the KEY holds
// (probe), keyInScope what its cancel-own Constraints allow for the execution.
func plannedKeyHandler(t *testing.T, keyCancelOwn, keyInScope bool) (*Handler, *MockConfigStore) {
	t.Helper()
	ctx := mock.Anything
	owner, keyID := revokeConstraintsUser, "key-1"
	exec := cancelRouteExec("pending")
	exec.CreatedByUserID = &owner
	store := new(MockConfigStore)
	t.Cleanup(func() { store.AssertExpectations(t) })
	store.On("GetExecutionByID", ctx, cancelExecID).Return(exec, nil)
	store.On("TransitionExecutionStatus", ctx, cancelExecID, mock.Anything, "canceled", mock.Anything).
		Return(&config.PurchaseExecution{ExecutionID: cancelExecID, Status: "canceled"}, nil).Maybe()
	store.On("DeleteSuppressionsByExecutionTx", ctx, ctx, cancelExecID).Return(nil).Maybe()
	m := new(MockAuthService)
	t.Cleanup(func() { m.AssertExpectations(t) })
	m.On("HasAPIKeyPermissionAPI", ctx, "user-key-value", auth.ActionDelete, auth.ResourcePurchases).Return(owner, keyID, true, nil)
	m.On("GetAllowedAccountsAPI", ctx, owner).Return([]string{}, nil)
	m.On("HasPermissionAPI", ctx, owner, mock.Anything, auth.ResourcePurchases).Return(false, nil).Maybe()
	m.On("HasPermissionAPI", ctx, owner, auth.ActionCancelOwn, auth.ResourcePurchases).Return(true, nil).Maybe()
	// The key holds neither full-scope verb (#979: probed on the key).
	m.On("HasAPIKeyPermissionForConstraintsAPI", ctx, keyID, owner, auth.ActionUpdateAny, auth.ResourcePurchases, []auth.PermissionConstraints{{}}).Return(false, nil)
	m.On("HasAPIKeyPermissionForConstraintsAPI", ctx, keyID, owner, auth.ActionCancelAny, auth.ResourcePurchases, []auth.PermissionConstraints{{}}).Return(false, nil)
	m.On("HasAPIKeyPermissionForConstraintsAPI", ctx, keyID, owner, auth.ActionCancelOwn, auth.ResourcePurchases, []auth.PermissionConstraints{{}}).Return(keyCancelOwn, nil)
	m.On("HasAPIKeyPermissionForConstraintsAPI", ctx, keyID, owner, auth.ActionCancelOwn, auth.ResourcePurchases, mock.Anything).Return(keyInScope, nil).Maybe()
	return &Handler{config: store, auth: m}, store
}

func TestDeletePlannedPurchase_APIKeyCancelOwnFollowsTheKey(t *testing.T) {
	t.Parallel()
	req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"x-api-key": "user-key-value"}}
	t.Run("delete-only key keeps working although the owner holds cancel-own", func(t *testing.T) {
		t.Parallel()
		h, _ := plannedKeyHandler(t, false, false)
		resp, err := h.deletePlannedPurchase(context.Background(), req, cancelExecID)
		require.NoError(t, err)
		assert.Equal(t, "canceled", resp.Status)
	})
	t.Run("key with constrained cancel-own out of scope is denied", func(t *testing.T) {
		t.Parallel()
		h, store := plannedKeyHandler(t, true, false)
		_, err := h.deletePlannedPurchase(context.Background(), req, cancelExecID)
		requireRevokeForbidden(t, err, constraintDenied)
		store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})
	t.Run("key with in-scope cancel-own is allowed", func(t *testing.T) {
		t.Parallel()
		h, _ := plannedKeyHandler(t, true, true)
		_, err := h.deletePlannedPurchase(context.Background(), req, cancelExecID)
		require.NoError(t, err)
	})
}

func TestDeletePlannedPurchase_ConstraintStoreErrorDoesNotFallBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	boom := errors.New("constraint store down")
	exec := cancelRouteExec("pending")
	owner := revokeConstraintsUser
	exec.CreatedByUserID = &owner
	store := new(MockConfigStore)
	t.Cleanup(func() { store.AssertExpectations(t) })
	store.On("GetExecutionByID", mock.Anything, cancelExecID).Return(exec, nil)
	m := new(MockAuthService)
	t.Cleanup(func() { m.AssertExpectations(t) })
	m.On("ValidateSession", mock.Anything, "sess-tok").Return(&Session{UserID: owner}, nil)
	m.On("GetAllowedAccountsAPI", mock.Anything, owner).Return([]string{}, nil)
	m.On("HasPermissionAPI", mock.Anything, owner, auth.ActionCancelAny, auth.ResourcePurchases).Return(true, nil)
	m.On("HasPermissionAPI", mock.Anything, owner, mock.Anything, auth.ResourcePurchases).Return(false, nil).Maybe()
	m.On("HasPermissionForConstraintsAPI", mock.Anything, owner, auth.ActionCancelAny, auth.ResourcePurchases, mock.Anything).Return(false, boom)
	// cancel-own is deliberately denied: a fallback would let the creator delete.
	h := &Handler{config: store, auth: m}
	_, err := h.deletePlannedPurchase(ctx, deletePlannedReq(), cancelExecID)
	require.ErrorIs(t, err, boom)
	store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestDeletePlannedPurchase_APIKeyVerbProbeErrorRefuses(t *testing.T) {
	t.Parallel()
	boom := errors.New("key store down")
	owner, keyID := revokeConstraintsUser, "key-1"
	exec := cancelRouteExec("pending")
	exec.CreatedByUserID = &owner
	store := new(MockConfigStore)
	t.Cleanup(func() { store.AssertExpectations(t) })
	store.On("GetExecutionByID", mock.Anything, cancelExecID).Return(exec, nil)
	m := new(MockAuthService)
	t.Cleanup(func() { m.AssertExpectations(t) })
	m.On("HasAPIKeyPermissionAPI", mock.Anything, "user-key-value", auth.ActionDelete, auth.ResourcePurchases).Return(owner, keyID, true, nil)
	m.On("GetAllowedAccountsAPI", mock.Anything, owner).Return([]string{}, nil)
	m.On("HasPermissionAPI", mock.Anything, owner, mock.Anything, auth.ResourcePurchases).Return(false, nil).Maybe()
	m.On("HasAPIKeyPermissionForConstraintsAPI", mock.Anything, keyID, owner, auth.ActionUpdateAny, auth.ResourcePurchases, []auth.PermissionConstraints{{}}).Return(false, nil)
	m.On("HasAPIKeyPermissionForConstraintsAPI", mock.Anything, keyID, owner, auth.ActionCancelAny, auth.ResourcePurchases, []auth.PermissionConstraints{{}}).Return(false, nil)
	m.On("HasAPIKeyPermissionForConstraintsAPI", mock.Anything, keyID, owner, auth.ActionCancelOwn, auth.ResourcePurchases, []auth.PermissionConstraints{{}}).Return(false, boom)

	h := &Handler{config: store, auth: m}
	req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"x-api-key": "user-key-value"}}
	_, err := h.deletePlannedPurchase(context.Background(), req, cancelExecID)
	require.ErrorIs(t, err, boom)
	store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}
