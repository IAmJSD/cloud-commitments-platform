package api

import (
	"context"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// The two global arming switches need an unconstrained execute:purchases
// grant. Per-account arming (plan arm, ladder config arm, ladder resolve)
// keeps accepting an account-constrained grant.

func switchWriter(execute *auth.PermissionConstraints) apgCaller {
	return apgCaller{perms: []auth.Permission{
		{Action: auth.ActionUpdate, Resource: auth.ResourceConfig},
		{Action: auth.ActionExecute, Resource: auth.ResourcePurchases, Constraints: execute},
	}}
}

func TestArmingSwitches_ConstrainedExecuteRefused(t *testing.T) {
	off := baseValidConfig()
	for name, held := range map[string]*auth.PermissionConstraints{
		"cap":      capOnly(),
		"account":  {AccountIDs: []string{lgAcct}},
		"provider": providerOnly(),
	} {
		for _, body := range []string{`{"auto_purchase_enabled":true}`, `{"ladder_execution_enabled":true}`} {
			t.Run(name+body, func(t *testing.T) {
				saved, err := apgConfigCall(t, switchWriter(held), off, body)

				apgRefused(t, err)
				assert.Nil(t, saved, "a refused switch must not be saved")
			})
		}
	}
}

func TestArmingSwitches_UnconstrainedExecuteStillArms(t *testing.T) {
	off := baseValidConfig()
	for _, body := range []string{`{"auto_purchase_enabled":true}`, `{"ladder_execution_enabled":true}`} {
		saved, err := apgConfigCall(t, switchWriter(nil), off, body)
		require.NoError(t, err, body)
		require.NotNil(t, saved, body)
	}
}

func TestArmingSwitches_ConstrainedExecuteMayDisarm(t *testing.T) {
	on := baseValidConfig()
	on.AutoPurchaseEnabled, on.LadderExecutionEnabled = true, true
	saved, err := apgConfigCall(t, switchWriter(capOnly()), on, `{"auto_purchase_enabled":false,"ladder_execution_enabled":false}`)
	require.NoError(t, err)
	require.NotNil(t, saved)
}

func TestArmingSwitches_AdminKeyStillRefused(t *testing.T) {
	saved, err := apgConfigCall(t, apgCaller{key: true}, baseValidConfig(), `{"auto_purchase_enabled":true}`)
	apgRefused(t, err)
	assert.Nil(t, saved)
}

func TestArmingSwitches_UserAPIKeyUsesGlobalSetAndKeyID(t *testing.T) {
	mockAuth := new(MockAuthService)
	mockAuth.On("HasAPIKeyPermissionAPI", mock.Anything, "user-key", "update", "config").Return("u2", "key-1", true, nil)
	mockAuth.On("HasAPIKeyPermissionAPI", mock.Anything, "user-key", "execute", "purchases").Return("u2", "key-1", true, nil)
	mockAuth.On("GetAllowedAccountsAPI", mock.Anything, "u2").Return([]string(nil), nil)
	// The config save also resolves the RI exchange grant ahead of the transaction.
	mockAuth.On("HasAPIKeyPermissionAPI", mock.Anything, "user-key", "execute", "ri-exchange").Return("u2", "key-1", true, nil)
	mockAuth.On("HasAPIKeyPermissionForConstraintsAPI", mock.Anything, "key-1", "u2", "execute", "ri-exchange",
		mock.MatchedBy(isGlobalSet)).Return(true, nil)
	mockAuth.On("HasAPIKeyPermissionForConstraintsAPI", mock.Anything, "key-1", "u2", "update", "config",
		mock.MatchedBy(isGlobalSet)).Return(true, nil)
	mockAuth.On("HasAPIKeyPermissionForConstraintsAPI", mock.Anything, "key-1", "u2", "execute", "purchases",
		mock.MatchedBy(isGlobalSet)).Return(false, nil).Once()
	store := autoGateStore(t, baseValidConfig())
	h := &Handler{config: store, auth: mockAuth}
	ctx := contextWithPrincipal(context.Background(), &Principal{Kind: PrincipalUserAPIKey, UserID: "u2", APIKeyID: "key-1"})

	_, err := h.updateConfig(ctx, &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"x-api-key": "user-key"}, Body: `{"auto_purchase_enabled":true}`})

	apgRefused(t, err)
	mockAuth.AssertExpectations(t)
	assert.False(t, apgCalled(store, "SaveGlobalConfig"))
}

// Non-leak: per-account arming stays open to an account-constrained grant.
func TestPerAccountArming_ConstrainedExecuteStillWorks(t *testing.T) {
	acctScoped := &auth.PermissionConstraints{AccountIDs: []string{lgAcct}}
	t.Run("ladder config arm for own account", func(t *testing.T) {
		caller := apgCaller{perms: []auth.Permission{
			{Action: auth.ActionUpdate, Resource: auth.ResourceConfig, Constraints: acctScoped},
			{Action: auth.ActionExecute, Resource: auth.ResourcePurchases, Constraints: acctScoped},
		}}
		store, err := lgCall(t, caller, nil, lgBody(true, "auto_approve", "1"))
		require.NoError(t, err)
		assert.True(t, apgCalled(store, "UpsertLadderConfig"))
	})
	t.Run("ladder config write without arming", func(t *testing.T) {
		caller := apgCaller{perms: []auth.Permission{{Action: auth.ActionUpdate, Resource: auth.ResourceConfig, Constraints: acctScoped}}}
		store, err := lgCall(t, caller, nil, lgBody(false, "email_approval", ""))
		require.NoError(t, err)
		assert.True(t, apgCalled(store, "UpsertLadderConfig"))
	})
	t.Run("ladder resolve", func(t *testing.T) {
		store := newResolveStore()
		store.account = lgAcct
		caller := apgCaller{perms: []auth.Permission{{Action: auth.ActionExecute, Resource: auth.ResourcePurchases, Constraints: acctScoped}}}
		require.NoError(t, resolveCall(t, caller, store, resolveProvenBody))
		assert.Equal(t, 1, store.called)
	})
	t.Run("plan arm", func(t *testing.T) {
		store := new(MockConfigStore)
		store.On("GetPurchasePlan", mock.Anything, apgPlanID).Return(apgStoredPlan(false, apgCap(1000)), nil)
		store.On("GetPlanAccounts", mock.Anything, apgPlanID).Return([]config.CloudAccount{{ID: lgAcct, Provider: "aws"}}, nil)
		store.On("UpdatePurchasePlan", mock.Anything, mock.Anything).Return(nil).Maybe()
		caller := apgCaller{perms: []auth.Permission{
			{Action: auth.ActionUpdate, Resource: auth.ResourcePlans},
			{Action: auth.ActionExecute, Resource: auth.ResourcePurchases, Constraints: acctScoped},
		}}
		h, req := caller.handlerAndReq(store, `{"auto_purchase":true}`)
		_, err := h.patchPlan(context.Background(), req, apgPlanID)
		require.NoError(t, err)
		assert.True(t, apgCalled(store, "UpdatePurchasePlan"))
	})
}
