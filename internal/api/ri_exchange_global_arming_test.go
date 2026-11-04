package api

import (
	"context"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
)

// RI-exchange auto mode arms exchanges for every account, so it needs an
// unconstrained execute:ri-exchange grant.

const riAutoBody = `{"mode":"auto","auto_exchange_enabled":true,"utilization_threshold":50,"lookback_days":30,"max_payment_per_exchange_usd":100,"max_payment_daily_usd":500}`

func riAutoCaller(exchange *auth.PermissionConstraints) []auth.Permission {
	return []auth.Permission{
		{Action: auth.ActionUpdate, Resource: auth.ResourceConfig},
		{Action: auth.ActionExecute, Resource: auth.ResourceRIExchange, Constraints: exchange},
	}
}

func TestRIExchangeAutoMode_ConstrainedExecuteRefused(t *testing.T) {
	for name, held := range map[string]*auth.PermissionConstraints{
		"cap":      capOnly(),
		"account":  {AccountIDs: []string{scopedInAccount}},
		"provider": providerOnly(),
	} {
		t.Run(name, func(t *testing.T) {
			store := autoGateStore(t, disarmedConfig())
			mockAuth := new(MockAuthService)
			mockAuth.On("ValidateSession", mock.Anything, autoGateToken).Return(&Session{UserID: autoGateUserID}, nil).Maybe()
			mockAuth.grantPermissions(riAutoCaller(held))
			h := &Handler{config: store, auth: mockAuth}
			req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer " + autoGateToken}, Body: riAutoBody}

			_, err := h.updateRIExchangeConfig(context.Background(), req)

			apgRefused(t, err)
			assert.False(t, wrote(store), "a refused write must not reach the store")
		})
	}
}

func TestRIExchangeAutoMode_UnconstrainedAndAdminKeyStillArm(t *testing.T) {
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, autoGateToken).Return(&Session{UserID: autoGateUserID}, nil).Maybe()
	mockAuth.grantPermissions(riAutoCaller(nil))
	store := autoGateStore(t, disarmedConfig())
	h := &Handler{config: store, auth: mockAuth, apiKey: "admin-key"}
	bearer := &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer " + autoGateToken}, Body: riAutoBody}
	_, err := h.updateRIExchangeConfig(context.Background(), bearer)
	require.NoError(t, err)

	store2 := autoGateStore(t, disarmedConfig())
	h2 := &Handler{config: store2, auth: new(MockAuthService), apiKey: "admin-key"}
	adminKey := &events.LambdaFunctionURLRequest{Headers: map[string]string{"x-api-key": "admin-key"}, Body: riAutoBody}
	_, err = h2.updateRIExchangeConfig(context.Background(), adminKey)
	require.NoError(t, err)
	assert.True(t, wrote(store2))
}

func TestRIExchangeAutoMode_UserAPIKeyUsesGlobalSetAndKeyID(t *testing.T) {
	mockAuth := new(MockAuthService)
	mockAuth.On("HasAPIKeyPermissionAPI", mock.Anything, "user-key", "update", "config").Return("u2", "key-1", true, nil)
	mockAuth.On("HasAPIKeyPermissionAPI", mock.Anything, "user-key", "execute", "ri-exchange").Return("u2", "key-1", true, nil)
	mockAuth.On("GetAllowedAccountsAPI", mock.Anything, "u2").Return([]string(nil), nil)
	mockAuth.On("HasAPIKeyPermissionForConstraintsAPI", mock.Anything, "key-1", "u2", "update", "config",
		mock.MatchedBy(isGlobalSet)).Return(true, nil)
	mockAuth.On("HasAPIKeyPermissionForConstraintsAPI", mock.Anything, "key-1", "u2", "execute", "ri-exchange",
		mock.MatchedBy(isGlobalSet)).Return(false, nil).Once()
	store := autoGateStore(t, disarmedConfig())
	h := &Handler{config: store, auth: mockAuth}
	ctx := contextWithPrincipal(context.Background(), &Principal{Kind: PrincipalUserAPIKey, UserID: "u2", APIKeyID: "key-1"})

	_, err := h.updateRIExchangeConfig(ctx, &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"x-api-key": "user-key"}, Body: riAutoBody})

	apgRefused(t, err)
	mockAuth.AssertExpectations(t)
	assert.False(t, wrote(store))
}
