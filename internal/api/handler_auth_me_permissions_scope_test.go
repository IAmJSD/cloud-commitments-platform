package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
)

// Issue #619: POST /api/recommendations/refresh refuses a caller whose account
// scope is restricted (#615), but the frontend fires that refresh
// automatically for stale data and had no way to tell a scoped caller apart,
// so scoped users saw a "refresh failed: permission denied" toast for an
// action they never asked for. GET /api/auth/me/permissions now reports
// unrestricted_account_access, resolved through the same gate the refresh
// enforces, so the frontend can skip the automatic refresh.

const (
	meScopeKeyOwnerID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	meScopeKeyID      = "key-619"
	meScopeRawKey     = "raw-user-key-619"
)

// meScopeAuth returns a session caller holding view:recommendations whose
// allowed_accounts is accounts (nil means unrestricted).
func meScopeAuth(accounts []string) *MockAuthService {
	m := new(MockAuthService)
	m.On("ValidateSession", mock.Anything, scopedToken).
		Return(&Session{UserID: scopedUserID}, nil).Maybe()
	m.On("GetUserPermissionsAPI", mock.Anything, scopedUserID).
		Return([]auth.APIPermission{{Action: auth.ActionView, Resource: auth.ResourceRecommendations}}, nil).Maybe()
	m.grantPermissionsScoped(viewRecommendationsOnly(), accounts)
	return m
}

func routeMePermissions(t *testing.T, h *Handler, req *events.LambdaFunctionURLRequest) *UserPermissionsResponse {
	t.Helper()
	res, err := NewRouter(h).Route(context.Background(), http.MethodGet, "/api/auth/me/permissions", req)
	require.NoError(t, err)
	out, ok := res.(*UserPermissionsResponse)
	require.True(t, ok, "got %T", res)
	return out
}

func TestGetCurrentUserPermissions_ScopedSession_ReportsRestricted(t *testing.T) {
	h := &Handler{auth: meScopeAuth([]string{scopedInAccount})}

	got := routeMePermissions(t, h, scopedRequest(""))

	assert.False(t, got.UnrestrictedAccountAccess, "a caller limited to a subset of accounts is not unrestricted")
	assert.Equal(t, []PermissionEntry{{Action: auth.ActionView, Resource: auth.ResourceRecommendations}}, got.Permissions,
		"the permission set is reported unchanged")
	assert.False(t, got.IsAdmin)
}

func TestGetCurrentUserPermissions_UnrestrictedSession_ReportsUnrestricted(t *testing.T) {
	h := &Handler{auth: meScopeAuth(nil)}

	got := routeMePermissions(t, h, scopedRequest(""))

	assert.True(t, got.UnrestrictedAccountAccess)
}

// The wire key is always present, so the frontend can rely on it being a
// boolean rather than treating a missing field as either answer.
func TestGetCurrentUserPermissions_ScopedSession_WireField(t *testing.T) {
	h := &Handler{auth: meScopeAuth([]string{scopedInAccount})}
	req := scopedRequest("")
	req.RequestContext.HTTP = events.LambdaFunctionURLRequestContextHTTPDescription{
		Method: http.MethodGet, Path: "/api/auth/me/permissions",
	}

	resp, err := h.HandleRequest(context.Background(), req)

	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", resp.Body)
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &body), "body: %s", resp.Body)
	assert.Equal(t, false, body["unrestricted_account_access"], "body: %s", resp.Body)
}

// A user API key is gated by its owner's account scope on the refresh
// (authorizeAPIKey puts the owner on the session), so it is reported the same.
func TestGetCurrentUserPermissions_UserAPIKey_ReportsOwnerScope(t *testing.T) {
	for name, tc := range map[string]struct {
		accounts []string
		want     bool
	}{
		"scoped owner":       {accounts: []string{scopedInAccount}, want: false},
		"unrestricted owner": {accounts: []string{}, want: true},
	} {
		t.Run(name, func(t *testing.T) {
			m := new(MockAuthService)
			t.Cleanup(func() { m.AssertExpectations(t) })
			m.On("ValidateUserAPIKeyAPI", mock.Anything, meScopeRawKey).
				Return(&auth.UserAPIKey{ID: meScopeKeyID}, &auth.User{ID: meScopeKeyOwnerID}, nil).Once()
			m.On("GetAPIKeyEffectivePermissionsAPI", mock.Anything, meScopeKeyID, meScopeKeyOwnerID).
				Return([]auth.APIPermission{{Action: auth.ActionView, Resource: auth.ResourceRecommendations}}, nil).Once()
			m.On("GetAllowedAccountsAPI", mock.Anything, meScopeKeyOwnerID).Return(tc.accounts, nil).Once()
			h := &Handler{auth: m}

			got := routeMePermissions(t, h, &events.LambdaFunctionURLRequest{
				Headers: map[string]string{"X-API-Key": meScopeRawKey},
			})

			assert.Equal(t, tc.want, got.UnrestrictedAccountAccess)
		})
	}
}

// A scope that cannot be established is reported as restricted (the
// conservative answer: the UI skips the automatic global refresh) without
// failing the permissions response the whole UI gates on.
func TestGetCurrentUserPermissions_ScopeLookupError_ReportsRestricted(t *testing.T) {
	m := new(MockAuthService)
	t.Cleanup(func() { m.AssertExpectations(t) })
	m.On("ValidateSession", mock.Anything, scopedToken).Return(&Session{UserID: scopedUserID}, nil).Once()
	m.On("GetUserPermissionsAPI", mock.Anything, scopedUserID).
		Return([]auth.APIPermission{{Action: auth.ActionView, Resource: auth.ResourceRecommendations}}, nil).Once()
	m.On("GetAllowedAccountsAPI", mock.Anything, scopedUserID).Return(nil, errors.New("scope lookup failed")).Once()
	h := &Handler{auth: m}

	got := routeMePermissions(t, h, scopedRequest(""))

	assert.False(t, got.UnrestrictedAccountAccess)
	assert.Equal(t, []PermissionEntry{{Action: auth.ActionView, Resource: auth.ResourceRecommendations}}, got.Permissions)
}

func TestGetCurrentUserPermissions_AdminAPIKey_ReportsUnrestricted(t *testing.T) {
	m := new(MockAuthService)
	h := &Handler{auth: m, apiKey: "admin-key-619"}

	got := routeMePermissions(t, h, &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"X-API-Key": "admin-key-619"},
	})

	assert.True(t, got.UnrestrictedAccountAccess)
	m.AssertNotCalled(t, "GetAllowedAccountsAPI", mock.Anything, mock.Anything)
}

// The reported flag and the refresh gate must not drift: for the same caller,
// the flag is true exactly when POST /api/recommendations/refresh gets past the
// scope check, and false exactly when it is refused with the scope 403.
func TestGetCurrentUserPermissions_FlagMatchesRefreshGate(t *testing.T) {
	t.Setenv("SCHEDULER_LAMBDA_ARN", "")
	for name, accounts := range map[string][]string{
		"scoped":                   {scopedInAccount},
		"scoped to two accounts":   {scopedInAccount, scopedOutAccount},
		"unrestricted (nil)":       nil,
		"unrestricted (empty)":     {},
		"wildcard allowed list":    {"*"},
		"wildcard plus an account": {"*", scopedInAccount},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRefreshFixtureWithAuth(t, meScopeAuth(accounts))

			got := routeMePermissions(t, f.handler, scopedRequest(""))
			res, refreshErr := f.refresh(scopedRequest(""))

			if got.UnrestrictedAccountAccess {
				require.NoError(t, refreshErr, "flag says unrestricted, so the refresh must pass the gate")
				assert.IsType(t, &RefreshResponse{}, res)
				return
			}
			requireForbiddenUnrestricted(t, res, refreshErr)
			f.assertNothingStarted(t)
		})
	}
}
