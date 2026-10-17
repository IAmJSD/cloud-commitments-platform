package api

import (
	"context"
	"errors"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #404: revoke-any / revoke-own Constraints (Providers, Services,
// Regions, AccountIDs) must bound what a holder can revoke. These tests answer
// from the real auth matcher via grantPermissionsScoped.

const revokeConstraintsUser = "u-1"

func revokePerm(action string, c *auth.PermissionConstraints) auth.Permission {
	return auth.Permission{Action: action, Resource: auth.ResourcePurchases, Constraints: c}
}

func revokeHistoryRecord() *config.PurchaseHistoryRecord {
	acct := "acct-uuid"
	return &config.PurchaseHistoryRecord{
		PurchaseID: "p-1", CloudAccountID: &acct, Provider: "azure", Service: "compute", Region: "westeurope",
	}
}

func newRevokeHandler(perms ...auth.Permission) *Handler {
	mockAuth := new(MockAuthService)
	mockAuth.grantPermissionsScoped(perms, nil)
	return &Handler{auth: mockAuth, config: &MockConfigStore{}}
}

func requireRevokeForbidden(t *testing.T, err error, contains string) {
	t.Helper()
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected ClientError, got %T: %v", err, err)
	assert.Equal(t, 403, ce.code)
	assert.Contains(t, ce.Error(), contains)
}

const constraintDenied = "exceeds the constraints"

func TestAuthorizeSessionRevoke_ConstraintDimensions(t *testing.T) {
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
	for _, verb := range []string{auth.ActionRevokeAny, auth.ActionRevokeOwn} {
		for dim, c := range denying {
			t.Run(verb+"/deny/"+dim, func(t *testing.T) {
				t.Parallel()
				h := newRevokeHandler(revokePerm(verb, c))
				err := h.authorizeSessionRevoke(context.Background(), &Session{UserID: revokeConstraintsUser}, revokeHistoryRecord())
				requireRevokeForbidden(t, err, constraintDenied)
			})
			t.Run(verb+"/allow/"+dim, func(t *testing.T) {
				t.Parallel()
				h := newRevokeHandler(revokePerm(verb, allowing[dim]))
				require.NoError(t, h.authorizeSessionRevoke(context.Background(), &Session{UserID: revokeConstraintsUser}, revokeHistoryRecord()))
			})
		}
	}
}

func TestAuthorizeSessionRevoke_StrictScopeUnknownDimensions(t *testing.T) {
	t.Parallel()
	t.Run("empty region denies a Regions grant", func(t *testing.T) {
		t.Parallel()
		h := newRevokeHandler(revokePerm(auth.ActionRevokeAny, &auth.PermissionConstraints{Regions: []string{"westeurope"}}))
		rec := revokeHistoryRecord()
		rec.Region = ""
		requireRevokeForbidden(t, h.authorizeSessionRevoke(context.Background(), &Session{UserID: revokeConstraintsUser}, rec), constraintDenied)
	})
	t.Run("unattributed account denies an AccountIDs grant", func(t *testing.T) {
		t.Parallel()
		h := newRevokeHandler(revokePerm(auth.ActionRevokeAny, &auth.PermissionConstraints{AccountIDs: []string{"acct-uuid"}}))
		rec := revokeHistoryRecord()
		rec.CloudAccountID = nil
		rec.AccountID = "123456789012"
		requireRevokeForbidden(t, h.authorizeSessionRevoke(context.Background(), &Session{UserID: revokeConstraintsUser}, rec), constraintDenied)
	})
	t.Run("unconstrained grant still allows an unattributed region", func(t *testing.T) {
		t.Parallel()
		h := newRevokeHandler(revokePerm(auth.ActionRevokeAny, nil))
		rec := revokeHistoryRecord()
		rec.Region = ""
		require.NoError(t, h.authorizeSessionRevoke(context.Background(), &Session{UserID: revokeConstraintsUser}, rec))
	})
}

func TestAuthorizeSessionRevoke_ConstrainedAnyFallsBackToOwn(t *testing.T) {
	t.Parallel()
	constrained := &auth.PermissionConstraints{Providers: []string{"aws"}}
	sess := &Session{UserID: revokeConstraintsUser}

	t.Run("unconstrained own is allowed", func(t *testing.T) {
		t.Parallel()
		h := newRevokeHandler(revokePerm(auth.ActionRevokeAny, constrained), revokePerm(auth.ActionRevokeOwn, nil))
		require.NoError(t, h.authorizeSessionRevoke(context.Background(), sess, revokeHistoryRecord()))
	})
	t.Run("constrained any without own is denied", func(t *testing.T) {
		t.Parallel()
		h := newRevokeHandler(revokePerm(auth.ActionRevokeAny, constrained))
		requireRevokeForbidden(t, h.authorizeSessionRevoke(context.Background(), sess, revokeHistoryRecord()), constraintDenied)
	})
	t.Run("both constrained and outside is denied", func(t *testing.T) {
		t.Parallel()
		h := newRevokeHandler(revokePerm(auth.ActionRevokeAny, constrained), revokePerm(auth.ActionRevokeOwn, constrained))
		requireRevokeForbidden(t, h.authorizeSessionRevoke(context.Background(), sess, revokeHistoryRecord()), constraintDenied)
	})
	t.Run("fallback re-runs the revoke-own account check", func(t *testing.T) {
		t.Parallel()
		h := newRevokeHandler(revokePerm(auth.ActionRevokeAny, constrained), revokePerm(auth.ActionRevokeOwn, nil))
		rec := revokeHistoryRecord()
		rec.CloudAccountID = nil // unattributed: revoke-any skips the check, revoke-own cannot
		requireRevokeForbidden(t, h.authorizeSessionRevoke(context.Background(), sess, rec), "cannot verify ownership")
	})
}

func TestAuthorizeSessionRevoke_ConstraintStoreErrorPropagatesWithoutFallback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	boom := errors.New("constraint store down")
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("HasPermissionAPI", ctx, revokeConstraintsUser, auth.ActionRevokeAny, auth.ResourcePurchases).Return(true, nil)
	mockAuth.On("GetAllowedAccountsAPI", ctx, revokeConstraintsUser).Return([]string{}, nil)
	mockAuth.On("HasPermissionForConstraintsAPI", ctx, revokeConstraintsUser, auth.ActionRevokeAny, auth.ResourcePurchases, mock.Anything).Return(false, boom)
	// revoke-own is deliberately unregistered: querying it fails the test.

	h := &Handler{auth: mockAuth, config: &MockConfigStore{}}
	err := h.authorizeSessionRevoke(ctx, &Session{UserID: revokeConstraintsUser}, revokeHistoryRecord())
	require.ErrorIs(t, err, boom)
	_, isClient := IsClientError(err)
	assert.False(t, isClient)
}

func TestAuthorizeSessionRevoke_ConstraintSetSentFromRecord(t *testing.T) {
	t.Parallel()
	want := []auth.PermissionConstraints{{
		StrictScope: true, AccountIDs: []string{"acct-uuid"}, Providers: []string{"azure"},
		Services: []string{"compute"}, Regions: []string{"westeurope"},
	}}
	for _, verb := range []string{auth.ActionRevokeAny, auth.ActionRevokeOwn} {
		t.Run(verb, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			mockAuth := new(MockAuthService)
			t.Cleanup(func() { mockAuth.AssertExpectations(t) })
			mockAuth.On("HasPermissionAPI", ctx, revokeConstraintsUser, auth.ActionRevokeAny, auth.ResourcePurchases).Return(verb == auth.ActionRevokeAny, nil)
			if verb == auth.ActionRevokeOwn {
				mockAuth.On("HasPermissionAPI", ctx, revokeConstraintsUser, auth.ActionRevokeOwn, auth.ResourcePurchases).Return(true, nil)
			}
			mockAuth.On("GetAllowedAccountsAPI", ctx, revokeConstraintsUser).Return([]string{"acct-uuid"}, nil)
			mockAuth.On("HasPermissionForConstraintsAPI", ctx, revokeConstraintsUser, verb, auth.ResourcePurchases, want).Return(true, nil).Once()

			h := &Handler{auth: mockAuth, config: &MockConfigStore{}}
			require.NoError(t, h.authorizeSessionRevoke(ctx, &Session{UserID: revokeConstraintsUser}, revokeHistoryRecord()))
		})
	}
}

func TestAuthorizeSessionRevoke_APIKeySessionUsesKeyConstraints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	// The verbs are probed on the key, not the owner (issue #1005): the key
	// holds revoke-any but not revoke-own, and its revoke-any Constraints deny.
	probe := []auth.PermissionConstraints{{}}
	mockAuth.On("HasAPIKeyPermissionForConstraintsAPI", ctx, "key-1", revokeConstraintsUser, auth.ActionRevokeAny, auth.ResourcePurchases, probe).Return(true, nil)
	mockAuth.On("HasAPIKeyPermissionForConstraintsAPI", ctx, "key-1", revokeConstraintsUser, auth.ActionRevokeOwn, auth.ResourcePurchases, probe).Return(false, nil)
	mockAuth.On("GetAllowedAccountsAPI", ctx, revokeConstraintsUser).Return([]string{}, nil)
	mockAuth.On("HasAPIKeyPermissionForConstraintsAPI", ctx, "key-1", revokeConstraintsUser, auth.ActionRevokeAny, auth.ResourcePurchases, mock.Anything).Return(false, nil)

	h := &Handler{auth: mockAuth, config: &MockConfigStore{}}
	err := h.authorizeSessionRevoke(ctx, &Session{UserID: revokeConstraintsUser, UserAPIKeyID: "key-1"}, revokeHistoryRecord())
	requireRevokeForbidden(t, err, constraintDenied)
}

func execWithRecs(creator string, recs ...config.RecommendationRecord) *config.PurchaseExecution {
	ex := scheduledExecution("exec-1", creator)
	ex.Recommendations = recs
	return ex
}

func recFor(account, provider, service, region string) config.RecommendationRecord {
	rec := config.RecommendationRecord{Provider: provider, Service: service, Region: region}
	if account != "" {
		rec.CloudAccountID = &account
	}
	return rec
}

func TestAuthorizeSessionRevokeExecution_ConstraintDimensions(t *testing.T) {
	t.Parallel()
	rec := recFor("acct-uuid", "azure", "compute", "westeurope")
	denying := map[string]*auth.PermissionConstraints{
		"providers":   {Providers: []string{"aws"}},
		"services":    {Services: []string{"rds"}},
		"regions":     {Regions: []string{"eastus"}},
		"account ids": {AccountIDs: []string{"other-acct"}},
	}
	for _, verb := range []string{auth.ActionRevokeAny, auth.ActionRevokeOwn} {
		for dim, c := range denying {
			t.Run(verb+"/"+dim, func(t *testing.T) {
				t.Parallel()
				h := newRevokeHandler(revokePerm(verb, c))
				err := h.authorizeSessionRevokeExecution(context.Background(), &Session{UserID: revokeConstraintsUser}, execWithRecs(revokeConstraintsUser, rec))
				requireRevokeForbidden(t, err, constraintDenied)
			})
		}
	}
}

func TestAuthorizeSessionRevokeExecution_EveryRecommendationMustBeCovered(t *testing.T) {
	t.Parallel()
	grant := revokePerm(auth.ActionRevokeAny, &auth.PermissionConstraints{Regions: []string{"westeurope"}})
	sess := &Session{UserID: revokeConstraintsUser}
	inside := recFor("a", "azure", "compute", "westeurope")
	outside := recFor("a", "azure", "compute", "eastus")

	h := newRevokeHandler(grant)
	require.NoError(t, h.authorizeSessionRevokeExecution(context.Background(), sess, execWithRecs("", inside, inside)))
	requireRevokeForbidden(t, h.authorizeSessionRevokeExecution(context.Background(), sess, execWithRecs("", inside, outside)), constraintDenied)
}

func TestAuthorizeSessionRevokeExecution_ZeroRecommendations(t *testing.T) {
	t.Parallel()
	sess := &Session{UserID: revokeConstraintsUser}
	t.Run("constrained grant denies", func(t *testing.T) {
		t.Parallel()
		h := newRevokeHandler(revokePerm(auth.ActionRevokeAny, &auth.PermissionConstraints{Providers: []string{"azure"}}))
		requireRevokeForbidden(t, h.authorizeSessionRevokeExecution(context.Background(), sess, execWithRecs("")), constraintDenied)
	})
	t.Run("unconstrained grant allows", func(t *testing.T) {
		t.Parallel()
		h := newRevokeHandler(revokePerm(auth.ActionRevokeAny, nil))
		require.NoError(t, h.authorizeSessionRevokeExecution(context.Background(), sess, execWithRecs("")))
	})
}

func TestAuthorizeSessionRevokeExecution_ConstrainedAnyFallsBackToOwn(t *testing.T) {
	t.Parallel()
	constrained := &auth.PermissionConstraints{Providers: []string{"aws"}}
	rec := recFor("a", "azure", "compute", "westeurope")
	sess := &Session{UserID: revokeConstraintsUser}

	t.Run("creator with unconstrained own is allowed", func(t *testing.T) {
		t.Parallel()
		h := newRevokeHandler(revokePerm(auth.ActionRevokeAny, constrained), revokePerm(auth.ActionRevokeOwn, nil))
		require.NoError(t, h.authorizeSessionRevokeExecution(context.Background(), sess, execWithRecs(revokeConstraintsUser, rec)))
	})
	t.Run("non-creator is denied by the creator check", func(t *testing.T) {
		t.Parallel()
		h := newRevokeHandler(revokePerm(auth.ActionRevokeAny, constrained), revokePerm(auth.ActionRevokeOwn, nil))
		requireRevokeForbidden(t, h.authorizeSessionRevokeExecution(context.Background(), sess, execWithRecs("someone-else", rec)), "another user's")
	})
	t.Run("constrained any without own is denied", func(t *testing.T) {
		t.Parallel()
		h := newRevokeHandler(revokePerm(auth.ActionRevokeAny, constrained))
		requireRevokeForbidden(t, h.authorizeSessionRevokeExecution(context.Background(), sess, execWithRecs(revokeConstraintsUser, rec)), constraintDenied)
	})
}

func TestAuthorizeSessionRevokeExecution_ConstraintSetPerRecommendation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	want := []auth.PermissionConstraints{
		{StrictScope: true, AccountIDs: []string{"a1"}, Providers: []string{"azure"}, Services: []string{"compute"}, Regions: []string{"westeurope"}},
		{StrictScope: true, Providers: []string{"aws"}, Services: []string{"rds"}},
	}
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("HasPermissionAPI", ctx, revokeConstraintsUser, auth.ActionRevokeAny, auth.ResourcePurchases).Return(true, nil)
	mockAuth.On("HasPermissionForConstraintsAPI", ctx, revokeConstraintsUser, auth.ActionRevokeAny, auth.ResourcePurchases, want).Return(true, nil).Once()

	h := &Handler{auth: mockAuth}
	ex := execWithRecs("", recFor("a1", "azure", "compute", "westeurope"), recFor("", "aws", "rds", ""))
	require.NoError(t, h.authorizeSessionRevokeExecution(ctx, &Session{UserID: revokeConstraintsUser}, ex))
}

// End to end through revokePurchase: a constrained grant that excludes the
// target must stop before any refund or cancel side effect.

func TestRevokePurchase_ConstrainedGrantNeverReachesAzure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockAuth := new(MockAuthService)
	r := armReservationRecord()
	acct := "acct-uuid"
	r.CloudAccountID = &acct
	r.Region = "westeurope"
	mockAuth.On("ValidateSession", ctx, "tok").Return(&Session{UserID: revokeConstraintsUser, Email: "u@example.com"}, nil)
	mockAuth.grantPermissionsScoped([]auth.Permission{
		revokePerm(auth.ActionRevokeAny, &auth.PermissionConstraints{Providers: []string{"aws"}}),
	}, nil)
	mockStore.On("GetExecutionByID", ctx, r.PurchaseID).Return(nil, config.ErrNotFound)
	mockStore.On("GetPurchaseHistoryByPurchaseID", ctx, r.PurchaseID).Return(r, nil)
	mockStore.On("MarkPurchaseRevoked", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	azureCalls := 0
	h := &Handler{
		config: mockStore, auth: mockAuth,
		azureRevokeFactory: &azureRevokeClientFactory{
			newCredential: func() (azcore.TokenCredential, error) {
				azureCalls++
				return nil, errors.New("azure must not be reached")
			},
		},
	}

	req := sessionReq("tok")
	req.Body = `{"expected_refund_amount":10,"expected_refund_currency":"USD"}`
	_, err := h.revokePurchase(ctx, req, r.PurchaseID)
	requireRevokeForbidden(t, err, constraintDenied)
	assert.Zero(t, azureCalls)
	mockStore.AssertNotCalled(t, "MarkPurchaseRevoked", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestRevokePurchase_ConstrainedGrantNeverCancelsScheduledExecution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", ctx, "tok").Return(&Session{UserID: revokeConstraintsUser, Email: "u@example.com"}, nil)
	mockAuth.grantPermissionsScoped([]auth.Permission{
		revokePerm(auth.ActionRevokeAny, &auth.PermissionConstraints{Regions: []string{"eastus"}}),
	}, nil)
	ex := execWithRecs("", recFor("a", "azure", "compute", "westeurope"))
	mockStore.On("GetExecutionByID", ctx, ex.ExecutionID).Return(ex, nil)
	mockStore.On("CancelScheduledExecutionAtomic", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(false, "", errors.New("must not be reached")).Maybe()

	h := &Handler{config: mockStore, auth: mockAuth}
	_, err := h.revokePurchase(ctx, sessionReq("tok"), ex.ExecutionID)
	requireRevokeForbidden(t, err, constraintDenied)
	mockStore.AssertNotCalled(t, "CancelScheduledExecutionAtomic", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}
