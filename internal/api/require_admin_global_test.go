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

// requireAdmin guards deployment-wide administration, so an {admin, *} grant
// that carries constraints does not satisfy it.

func adminHandler(t *testing.T, held *auth.PermissionConstraints, accounts []string) (*Handler, *MockConfigStore) {
	t.Helper()
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, scopedToken).Return(&Session{UserID: scopedUserID}, nil).Maybe()
	mockAuth.grantPermissionsScoped([]auth.Permission{{Action: auth.ActionAdmin, Resource: auth.ResourceAll, Constraints: held}}, accounts)
	store := new(MockConfigStore)
	return &Handler{config: store, auth: mockAuth}, store
}

func TestRequireAdmin_ConstrainedAdminRefused(t *testing.T) {
	for name, held := range map[string]*auth.PermissionConstraints{
		"account":  {AccountIDs: []string{scopedInAccount}},
		"provider": providerOnly(),
		"cap":      capOnly(),
		"region":   {Regions: []string{"us-east-1"}},
	} {
		t.Run(name, func(t *testing.T) {
			mockAuth := new(MockAuthService)
			mockAuth.On("ValidateSession", mock.Anything, scopedToken).Return(&Session{UserID: scopedUserID}, nil).Maybe()
			mockAuth.grantPermissionsScoped([]auth.Permission{{Action: auth.ActionAdmin, Resource: auth.ResourceAll, Constraints: held}}, nil)
			h := &Handler{config: new(MockConfigStore), auth: mockAuth}

			session, err := h.requireAdmin(context.Background(), scopedRequest(""))

			require.Error(t, err, "got session %v", session)
			ce, ok := IsClientError(err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, 403, ce.code)
		})
	}
}

func TestRequireAdmin_UnconstrainedAdminAllowedEvenWithAccountScope(t *testing.T) {
	for _, accounts := range [][]string{nil, {scopedInAccount}} {
		h, _ := adminHandler(t, nil, accounts)
		session, err := h.requireAdmin(context.Background(), scopedRequest(""))
		require.NoError(t, err)
		assert.Equal(t, scopedUserID, session.UserID)
	}
}

func TestRequireAdmin_AdminKeyKeepsBypass(t *testing.T) {
	h, _ := adminHandler(t, capOnly(), nil)
	h.apiKey = "admin-key"
	session, err := h.requireAdmin(context.Background(), &events.LambdaFunctionURLRequest{Headers: map[string]string{"x-api-key": "admin-key"}})
	require.NoError(t, err)
	assert.Equal(t, apiKeyAdminUserID, session.UserID)
}

func TestRouterDispatch_AdminRoute_ConstrainedAdminRefused(t *testing.T) {
	h, store := adminHandler(t, &auth.PermissionConstraints{AccountIDs: []string{scopedInAccount}}, nil)

	res, err := NewRouter(h).Route(context.Background(), "PUT", "/api/config", scopedRequest(`{}`))

	require.Error(t, err, "got response %v", res)
	ce, ok := IsClientError(err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, 403, ce.code)
	assert.Empty(t, store.Calls)
}

// The SourceIdentity probe in getConfig inherits the rule: a constrained admin
// reads the config (200) but gets no host identity.
func TestGetConfig_ConstrainedAdminGetsNoSourceIdentity(t *testing.T) {
	t.Setenv("CUDLY_SOURCE_CLOUD", "gcp")
	for _, tc := range []struct {
		name string
		held *auth.PermissionConstraints
		want bool
	}{
		{"unconstrained admin", nil, true},
		{"constrained admin", providerOnly(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, store := adminHandler(t, tc.held, nil)
			store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{}, nil)
			store.On("ListServiceConfigs", mock.Anything).Return([]config.ServiceConfig{}, nil)

			res, err := h.getConfig(context.Background(), scopedRequest(""))

			require.NoError(t, err)
			assert.Equal(t, tc.want, res.SourceIdentity != nil)
		})
	}
}
