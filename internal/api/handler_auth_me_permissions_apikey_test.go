package api //nolint:revive // var-naming: package name "api" is intentional for handler package

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
)

// Issue #62: GET /api/auth/me/permissions must describe a user API key by its
// EFFECTIVE permissions (key ∩ owner), not by its owner's full set. These
// tests drive HandleRequest (middleware, router, handler) into a real
// auth.Service backed by auth.MockStore, so the reported set comes from the
// same ComputeEffectivePermissions path that enforcement uses.

const (
	mePermsOwnerID = "aaaa0062-0000-4000-8000-000000000062"
	mePermsKeyID   = "key-62"
	mePermsRawKey  = "raw-user-key-62"
	mePermsToken   = "owner-session-62"
	mePermsGroupID = "group-62"
	mePermsAdmin   = "admin-api-key-62"
)

var adminAll = auth.Permission{Action: auth.ActionAdmin, Resource: auth.ResourceAll}

// mePermsAuth sends API-key validation, permission listing and permission
// decisions to a real auth.Service; only bearer-session validation is mocked.
type mePermsAuth struct {
	*MockAuthService
	service *auth.Service
	store   *auth.MockStore
}

func (a *mePermsAuth) ValidateUserAPIKeyAPI(ctx context.Context, apiKey string) (any, any, error) {
	key, user, err := a.service.ValidateUserAPIKeyAPI(ctx, apiKey)
	if err != nil {
		return nil, nil, err
	}
	return key, user, nil
}

func (a *mePermsAuth) GetAPIKeyEffectivePermissionsAPI(ctx context.Context, keyID, userID string) ([]auth.APIPermission, error) {
	return a.service.GetAPIKeyEffectivePermissionsAPI(ctx, keyID, userID)
}

func (a *mePermsAuth) GetUserPermissionsAPI(ctx context.Context, userID string) (any, error) {
	return a.service.GetUserPermissionsAPI(ctx, userID)
}

func (a *mePermsAuth) HasAPIKeyPermissionAPI(ctx context.Context, apiKey, action, resource string) (userID, keyID string, allowed bool, err error) {
	return a.service.HasAPIKeyPermissionAPI(ctx, apiKey, action, resource)
}

// keyLookup is what the store's GetAPIKeyByID answers.
type keyLookup struct {
	key *auth.UserAPIKey
	err error
}

// newMePermsAuth builds an owner whose group holds ownerPerms and a key
// scoped to keyPerms. keyByID, when non-nil, overrides what GetAPIKeyByID
// returns, so a test can make the key go bad after the middleware accepted it.
func newMePermsAuth(ownerPerms, keyPerms []auth.Permission, keyByID func(*auth.UserAPIKey) keyLookup) *mePermsAuth {
	sum := sha256.Sum256([]byte(mePermsRawKey))
	expires := time.Now().Add(24 * time.Hour)
	key := &auth.UserAPIKey{
		ID: mePermsKeyID, UserID: mePermsOwnerID, KeyHash: base64.RawURLEncoding.EncodeToString(sum[:]),
		IsActive: true, ExpiresAt: &expires, Permissions: keyPerms,
	}
	store := new(auth.MockStore)
	store.On("GetAPIKeyByHash", mock.Anything, key.KeyHash).Return(key, nil).Maybe()
	if keyByID == nil {
		store.On("GetAPIKeyByID", mock.Anything, mePermsKeyID).Return(key, nil).Maybe()
	} else {
		l := keyByID(key)
		store.On("GetAPIKeyByID", mock.Anything, mePermsKeyID).Return(l.key, l.err).Maybe()
	}
	store.On("GetUserByID", mock.Anything, mePermsOwnerID).Return(&auth.User{
		ID: mePermsOwnerID, Email: "owner@example.com", Active: true, GroupIDs: []string{mePermsGroupID},
	}, nil).Maybe()
	store.On("GetGroup", mock.Anything, mePermsGroupID).Return(&auth.Group{
		ID: mePermsGroupID, Permissions: ownerPerms,
	}, nil).Maybe()

	m := new(MockAuthService)
	m.On("ValidateSession", mock.Anything, mePermsToken).Return(&Session{UserID: mePermsOwnerID, Email: "owner@example.com"}, nil).Maybe()
	m.On("ValidateCSRFToken", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	m.On("GetAllowedAccountsAPI", mock.Anything, mePermsOwnerID).Return([]string{}, nil).Maybe()
	return &mePermsAuth{
		MockAuthService: m,
		service:         auth.NewService(auth.ServiceConfig{Store: store, CSRFKey: auth.TestCSRFKey()}),
		store:           store,
	}
}

func getMePermissions(t *testing.T, a AuthServiceInterface, headers map[string]string) *events.LambdaFunctionURLResponse {
	t.Helper()
	h := &Handler{auth: a, apiKey: mePermsAdmin}
	resp, err := h.HandleRequest(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: headers,
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: http.MethodGet, Path: "/api/auth/me/permissions"},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	return resp
}

func decodeMePermissions(t *testing.T, resp *events.LambdaFunctionURLResponse) UserPermissionsResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", resp.Body)
	var out UserPermissionsResponse
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &out), "body: %s", resp.Body)
	return out
}

// storeCalled reports whether the auth store recorded a call to method.
func storeCalled(store *auth.MockStore, method string) bool {
	for _, c := range store.Calls {
		if c.Method == method {
			return true
		}
	}
	return false
}

func meKeyHeaders() map[string]string { return map[string]string{"X-API-Key": mePermsRawKey} }

func meOwnerHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + mePermsToken}
}

// assertReportedMatchesEnforced checks that every reported entry is one the
// key is actually allowed on the request path, and that admin is held only
// when reported, so the advertised and enforced sets cannot drift (#61/#62).
func assertReportedMatchesEnforced(t *testing.T, a *mePermsAuth, got UserPermissionsResponse) {
	t.Helper()
	for _, p := range got.Permissions {
		_, _, allowed, err := a.HasAPIKeyPermissionAPI(context.Background(), mePermsRawKey, p.Action, p.Resource)
		require.NoError(t, err)
		assert.True(t, allowed, "reported %s:%s must be enforced as allowed", p.Action, p.Resource)
	}
	_, _, adminAllowed, err := a.HasAPIKeyPermissionAPI(context.Background(), mePermsRawKey, auth.ActionAdmin, auth.ResourceAll)
	require.NoError(t, err)
	assert.Equal(t, adminAllowed, got.IsAdmin, "is_admin must match what enforcement grants the key")
}

// TestGetCurrentUserPermissions_UserAPIKey_ReturnsEffectiveScope is the #62
// regression: an admin's view-only key is reported as view-only, not admin.
func TestGetCurrentUserPermissions_UserAPIKey_ReturnsEffectiveScope(t *testing.T) {
	view := auth.Permission{Action: auth.ActionView, Resource: auth.ResourceRecommendations}
	a := newMePermsAuth([]auth.Permission{adminAll}, []auth.Permission{view}, nil)

	got := decodeMePermissions(t, getMePermissions(t, a, meKeyHeaders()))

	assert.Equal(t, []PermissionEntry{{Action: auth.ActionView, Resource: auth.ResourceRecommendations}}, got.Permissions)
	assert.False(t, got.IsAdmin, "a view-only key must not be reported as admin")
	assertReportedMatchesEnforced(t, a, got)
}

// A key sent together with its owner's bearer session is still evaluated as
// the key by requirePermission, so it must be reported as the key too.
func TestGetCurrentUserPermissions_UserAPIKeyWithOwnerSession_ReportsKey(t *testing.T) {
	view := auth.Permission{Action: auth.ActionView, Resource: auth.ResourceRecommendations}
	a := newMePermsAuth([]auth.Permission{adminAll}, []auth.Permission{view}, nil)
	headers := meKeyHeaders()
	for k, v := range meOwnerHeaders() {
		headers[k] = v
	}

	got := decodeMePermissions(t, getMePermissions(t, a, headers))

	assert.Equal(t, []PermissionEntry{{Action: auth.ActionView, Resource: auth.ResourceRecommendations}}, got.Permissions)
	assert.False(t, got.IsAdmin)
}

// The owner's own session is unchanged: it still reports the owner's groups.
func TestGetCurrentUserPermissions_OwnerSession_Unchanged(t *testing.T) {
	view := auth.Permission{Action: auth.ActionView, Resource: auth.ResourceRecommendations}
	a := newMePermsAuth([]auth.Permission{adminAll}, []auth.Permission{view}, nil)

	got := decodeMePermissions(t, getMePermissions(t, a, meOwnerHeaders()))

	assert.Equal(t, []PermissionEntry{{Action: auth.ActionAdmin, Resource: auth.ResourceAll}}, got.Permissions)
	assert.True(t, got.IsAdmin)
	assert.False(t, storeCalled(a.store, "GetAPIKeyByID"), "a session must not be described through a key")
}

// The stateless admin API key is unchanged: {admin, *} with no store lookup.
func TestGetCurrentUserPermissions_AdminAPIKey_Unchanged(t *testing.T) {
	a := newMePermsAuth([]auth.Permission{adminAll}, nil, nil)

	got := decodeMePermissions(t, getMePermissions(t, a, map[string]string{"X-API-Key": mePermsAdmin}))

	assert.Equal(t, []PermissionEntry{{Action: auth.ActionAdmin, Resource: auth.ResourceAll}}, got.Permissions)
	assert.True(t, got.IsAdmin)
	assert.False(t, storeCalled(a.store, "GetAPIKeyByID"), "the admin API key must not look up a key row")
	assert.False(t, storeCalled(a.store, "GetUserByID"), "the admin API key must not look up a user row")
}

// A deliberately admin-scoped key on an admin owner keeps admin, so is_admin
// follows the effective set rather than being forced off for every key.
func TestGetCurrentUserPermissions_AdminScopedKey_ReportsAdmin(t *testing.T) {
	a := newMePermsAuth([]auth.Permission{adminAll}, []auth.Permission{adminAll}, nil)

	got := decodeMePermissions(t, getMePermissions(t, a, meKeyHeaders()))

	assert.Equal(t, []PermissionEntry{{Action: auth.ActionAdmin, Resource: auth.ResourceAll}}, got.Permissions)
	assert.True(t, got.IsAdmin)
	assertReportedMatchesEnforced(t, a, got)
}

// A constrained key is reported by its own entries: key permissions the
// owner lacks are dropped, the owner's extra permissions are not added, and
// the surviving entry is the key's (its constraints are the key's; the wire
// shape carries action/resource only, see the auth-level test for the
// constraint values).
func TestGetCurrentUserPermissions_ConstrainedKey_ReportsKeyEntries(t *testing.T) {
	ownerPerms := []auth.Permission{
		{Action: auth.ActionExecute, Resource: auth.ResourcePurchases, Constraints: &auth.PermissionConstraints{MaxPurchaseAmount: 1000}},
		{Action: auth.ActionView, Resource: auth.ResourceRecommendations},
		{Action: auth.ActionView, Resource: auth.ResourcePurchases},
	}
	keyPerms := []auth.Permission{
		{Action: auth.ActionExecute, Resource: auth.ResourcePurchases,
			Constraints: &auth.PermissionConstraints{MaxPurchaseAmount: 100, Providers: []string{"aws"}}},
		{Action: auth.ActionDelete, Resource: auth.ResourceUsers}, // owner lacks it
	}
	a := newMePermsAuth(ownerPerms, keyPerms, nil)

	got := decodeMePermissions(t, getMePermissions(t, a, meKeyHeaders()))

	assert.Equal(t, []PermissionEntry{{Action: auth.ActionExecute, Resource: auth.ResourcePurchases}}, got.Permissions)
	assert.False(t, got.IsAdmin)
	assertReportedMatchesEnforced(t, a, got)

	eff, err := a.GetAPIKeyEffectivePermissionsAPI(context.Background(), mePermsKeyID, mePermsOwnerID)
	require.NoError(t, err)
	require.Len(t, eff, 1)
	require.NotNil(t, eff[0].Constraints)
	assert.InDelta(t, 100, eff[0].Constraints.MaxAmount, 0, "the key's cap, not the owner's")
	assert.Equal(t, []string{"aws"}, eff[0].Constraints.Providers)
}

// When the key's effective permissions cannot be established after the
// middleware accepted it, the request is refused; it never falls back to
// the owner's permissions.
func TestGetCurrentUserPermissions_UserAPIKeyLookupFailureRefuses(t *testing.T) {
	view := auth.Permission{Action: auth.ActionView, Resource: auth.ResourceRecommendations}
	cases := map[string]func(*auth.UserAPIKey) keyLookup{
		"store error": func(*auth.UserAPIKey) keyLookup {
			return keyLookup{err: errors.New("db down")}
		},
		"revoked mid-request": func(k *auth.UserAPIKey) keyLookup {
			revoked := *k
			revoked.IsActive = false
			return keyLookup{key: &revoked}
		},
	}
	for name, keyByID := range cases {
		t.Run(name, func(t *testing.T) {
			a := newMePermsAuth([]auth.Permission{adminAll}, []auth.Permission{view}, keyByID)

			resp := getMePermissions(t, a, meKeyHeaders())

			assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, "body: %s", resp.Body)
			assert.NotContains(t, resp.Body, `"is_admin"`)
			assert.NotContains(t, resp.Body, auth.ActionAdmin)
		})
	}
}
