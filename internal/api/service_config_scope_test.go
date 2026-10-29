package api

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// PUT /api/config/service/{provider}/{service} writes the deployment-wide
// purchasing defaults (enabled, term, payment, coverage), so it carries the
// same all-accounts gate as PUT /api/config (issue #520).

const serviceConfigPath = "/api/config/service/aws/ec2"

const serviceConfigBody = `{"enabled":true,"term":1,"payment":"all-upfront","coverage":80}`

func serviceConfigSaveCalls(store *MockConfigStore) int {
	n := 0
	for _, c := range store.Calls {
		if c.Method == "SaveServiceConfig" {
			n++
		}
	}
	return n
}

func putServiceConfig(h *Handler) (any, error) {
	return NewRouter(h).Route(context.Background(), "PUT", serviceConfigPath, scopedRequest(serviceConfigBody))
}

func TestRouterDispatch_UpdateServiceConfig_ScopedUserRefused(t *testing.T) {
	h, store := scopedHandler(t, scopedInAccount)
	store.On("GetServiceConfig", mock.Anything, "aws", "ec2").Return(nil, nil).Maybe()
	store.On("SaveServiceConfig", mock.Anything, mock.Anything).Return(nil).Maybe()

	res, err := putServiceConfig(h)

	require.Error(t, err, "got response %v", res)
	assert.Contains(t, err.Error(), "global configuration requires unrestricted account access")
	assert.Zero(t, serviceConfigSaveCalls(store), "a scoped user must not reach the store write")
	assert.Empty(t, store.Calls, "a scoped user must not reach any store call")
}

func TestRouterDispatch_UpdateServiceConfig_SameRefusalAsGlobalConfig(t *testing.T) {
	h, _ := scopedHandler(t, scopedInAccount)

	_, globalErr := NewRouter(h).Route(context.Background(), "PUT", "/api/config", scopedRequest(`{}`))
	_, serviceErr := putServiceConfig(h)

	require.Error(t, globalErr)
	require.Error(t, serviceErr)
	assert.Equal(t, globalErr.Error(), serviceErr.Error())
	assert.Equal(t, globalErr, serviceErr)
}

func TestRouterDispatch_UpdateServiceConfig_UnrestrictedAdminSucceeds(t *testing.T) {
	h, store := scopedHandler(t)
	t.Cleanup(func() { store.AssertExpectations(t) })
	store.On("GetServiceConfig", mock.Anything, "aws", "ec2").Return(nil, nil)
	store.On("SaveServiceConfig", mock.Anything, mock.Anything).Return(nil)

	res, err := putServiceConfig(h)

	require.NoError(t, err)
	assert.Equal(t, &StatusResponse{Status: "updated"}, res)
	assert.Equal(t, 1, serviceConfigSaveCalls(store))
}

func TestUpdateServiceConfig_PermissionCheckPrecedesScopeCheck(t *testing.T) {
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, scopedToken).
		Return(&Session{UserID: scopedUserID}, nil).Maybe()
	mockAuth.grantPermissionsScoped([]auth.Permission{{Action: auth.ActionView, Resource: auth.ResourceConfig}},
		[]string{scopedInAccount})
	store := new(MockConfigStore)
	h := &Handler{config: store, auth: mockAuth}

	res, err := h.updateServiceConfig(context.Background(), scopedRequest(serviceConfigBody), "aws/ec2")

	require.Error(t, err, "got response %v", res)
	assert.Contains(t, err.Error(), "permission denied")
	assert.NotContains(t, err.Error(), "global configuration requires")
	assert.Empty(t, store.Calls)
}

func TestRouterDispatch_UpdateServiceConfig_ScopeRefusedBeforeBodyAndPathValidation(t *testing.T) {
	cases := map[string]struct{ path, body string }{
		"invalid body": {serviceConfigPath, `{`},
		"invalid path": {"/api/config/service/aws/..%2Fec2", serviceConfigBody},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h, store := scopedHandler(t, scopedInAccount)

			res, err := NewRouter(h).Route(context.Background(), "PUT", tc.path, scopedRequest(tc.body))

			require.Error(t, err, "got response %v", res)
			assert.Contains(t, err.Error(), "global configuration requires unrestricted account access")
			assert.Empty(t, store.Calls)
		})
	}
}

// An unrestricted account scope is not enough when the permission itself
// carries constraints: requireGlobalConfigScope's StrictScope check refuses it.
func TestUpdateServiceConfig_ConstrainedPermissionRefusedByStrictScope(t *testing.T) {
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, scopedToken).
		Return(&Session{UserID: scopedUserID}, nil).Maybe()
	mockAuth.grantPermissionsScoped([]auth.Permission{{
		Action: auth.ActionUpdate, Resource: auth.ResourceConfig,
		Constraints: &auth.PermissionConstraints{Providers: []string{"aws"}},
	}}, nil)
	store := new(MockConfigStore)
	h := &Handler{config: store, auth: mockAuth}

	res, err := h.updateServiceConfig(context.Background(), scopedRequest(serviceConfigBody), "aws/ec2")

	require.Error(t, err, "got response %v", res)
	assert.Empty(t, store.Calls)
}

func TestUpdateServiceConfig_NoSessionRefused(t *testing.T) {
	h, store := scopedHandler(t, scopedInAccount)
	req := &events.LambdaFunctionURLRequest{Body: serviceConfigBody}

	_, err := h.updateServiceConfig(context.Background(), req, "aws/ec2")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no authorization token")
	assert.Empty(t, store.Calls)
}
