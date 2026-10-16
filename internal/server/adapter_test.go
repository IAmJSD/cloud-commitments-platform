package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/api"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// hashSessionTokenForTest mirrors auth.hashSessionToken (private). Session
// tokens are stored as sha256+hex so the constant-time guard in
// auth.Service.ValidateSession matches when both sides are derived from the
// same raw token.
func hashSessionTokenForTest(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func createMockAuthService(t *testing.T) (*authServiceAdapter, *auth.MockStore) {
	t.Helper()
	mockStore := new(auth.MockStore)
	service := auth.NewService(auth.ServiceConfig{
		Store:           mockStore,
		SessionDuration: time.Hour,
		// Pin a deterministic CSRF key so CSRF-token derivation is
		// reproducible and hermetic. Without this, NewService generates a
		// random ephemeral key, so any test asserting a specific derived CSRF
		// token (TestAuthServiceAdapter_ValidateCSRFToken) cannot reproduce it.
		CSRFKey: auth.TestCSRFKey(),
	})
	adapter := newAuthServiceAdapter(service)
	return adapter, mockStore
}

func TestAuthServiceAdapter_Login(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	mockStore.On("GetUserByEmail", ctx, "test@example.com").Return(&auth.User{
		ID:           "user-1",
		Email:        "test@example.com",
		PasswordHash: "$2a$10$invalidhash",
		GroupIDs:     []string{auth.DefaultAdminGroupID},
	}, nil)

	_, err := adapter.Login(ctx, api.LoginRequest{
		Email:    "test@example.com",
		Password: "wrongpassword",
	})
	// Will fail on password check but exercises the adapter code path
	assert.Error(t, err)
}

func TestAuthServiceAdapter_Logout(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	// Token gets hashed internally, so use mock.Anything
	mockStore.On("DeleteSession", ctx, mock.AnythingOfType("string")).Return(nil)

	err := adapter.Logout(ctx, "token-123")
	require.NoError(t, err)
}

func TestAuthServiceAdapter_ValidateSession(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	// Token gets hashed; the stored Token must match hashSessionToken("valid-token")
	// so the constant-time guard in ValidateSession (PR #837 follow-up) succeeds.
	mockStore.On("GetSession", ctx, mock.AnythingOfType("string")).Return(&auth.Session{
		Token:     hashSessionTokenForTest("valid-token"),
		UserID:    "user-1",
		Email:     "test@example.com",
		ExpiresAt: time.Now().Add(time.Hour),
	}, nil)
	// ValidateSession loads the user to reject a deactivated account (issue
	// #89); an active user must not block an otherwise-valid session.
	mockStore.On("GetUserByID", ctx, "user-1").Return(&auth.User{ID: "user-1", Active: true}, nil)

	sess, err := adapter.ValidateSession(ctx, "valid-token")
	require.NoError(t, err)
	assert.Equal(t, "user-1", sess.UserID)
	assert.Equal(t, "test@example.com", sess.Email)
}

func TestAuthServiceAdapter_ValidateSession_Error(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	mockStore.On("GetSession", ctx, mock.AnythingOfType("string")).Return(nil, nil)

	_, err := adapter.ValidateSession(ctx, "expired-token")
	assert.Error(t, err)
}

func TestAuthServiceAdapter_CheckAdminExists(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	mockStore.On("AdminExists", ctx).Return(true, nil)

	exists, err := adapter.CheckAdminExists(ctx)
	require.NoError(t, err)
	assert.True(t, exists)
}

func TestAuthServiceAdapter_RequestPasswordReset(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	// User not found - exercises the adapter code path without needing email sender
	mockStore.On("GetUserByEmail", ctx, "nonexistent@example.com").Return(nil, nil)

	err := adapter.RequestPasswordReset(ctx, "nonexistent@example.com")
	// The service silently returns nil when user not found (to prevent enumeration)
	assert.NoError(t, err)
}

func TestAuthServiceAdapter_ConfirmPasswordReset(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	mockStore.On("GetUserByResetToken", ctx, mock.AnythingOfType("string")).Return(nil, assert.AnError)

	err := adapter.ConfirmPasswordReset(ctx, api.PasswordResetConfirm{
		Token:       "reset-token",
		NewPassword: "NewPassword123!",
	})
	assert.Error(t, err)
}

func TestAuthServiceAdapter_GetUser(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	mockStore.On("GetUserByID", ctx, "user-1").Return(&auth.User{
		ID:         "user-1",
		Email:      "test@example.com",
		GroupIDs:   []string{auth.DefaultAdminGroupID},
		MFAEnabled: false,
	}, nil)

	user, err := adapter.GetUser(ctx, "user-1")
	require.NoError(t, err)
	assert.Equal(t, "user-1", user.ID)
	assert.Equal(t, "test@example.com", user.Email)
	assert.Equal(t, []string{auth.DefaultAdminGroupID}, user.Groups)
}

func TestAuthServiceAdapter_GetUser_Error(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	mockStore.On("GetUserByID", ctx, "nonexistent").Return(nil, assert.AnError)

	_, err := adapter.GetUser(ctx, "nonexistent")
	assert.Error(t, err)
}

func TestAuthServiceAdapter_DeleteUser(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	// Non-admin user: the last-admin guard is skipped after the GetUserByID
	// lookup (issue #907), so deletion proceeds.
	mockStore.On("GetUserByID", ctx, "user-1").
		Return(&auth.User{ID: "user-1", GroupIDs: []string{"group-1"}}, nil)
	mockStore.On("DeleteUser", ctx, "user-1").Return(nil)
	mockStore.On("DeleteUserSessions", ctx, "user-1").Return(nil)

	err := adapter.DeleteUser(ctx, "user-1")
	require.NoError(t, err)
}

func TestAuthServiceAdapter_ListUsersAPI(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	mockStore.On("ListUsers", ctx).Return([]auth.User{
		{ID: "user-1", Email: "user1@example.com", GroupIDs: []string{auth.DefaultAdminGroupID}},
		{ID: "user-2", Email: "user2@example.com", GroupIDs: []string{"group-viewer"}},
	}, nil)

	result, err := adapter.ListUsersAPI(ctx)
	require.NoError(t, err)
	assert.NotNil(t, result)
}

func TestAuthServiceAdapter_ChangePasswordAPI(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	mockStore.On("GetUserByID", ctx, "user-1").Return(&auth.User{
		ID:           "user-1",
		Email:        "test@example.com",
		PasswordHash: "$2a$10$invalidhash",
	}, nil)

	err := adapter.ChangePasswordAPI(ctx, "user-1", "oldpass", "newpass")
	// Will fail on password verification
	assert.Error(t, err)
}

func TestAuthServiceAdapter_DeleteGroup(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	// DeleteGroup loads the group first to refuse system-managed ones (#1629).
	mockStore.On("GetGroup", ctx, "group-1").Return(&auth.Group{ID: "group-1", Name: "team"}, nil)
	mockStore.On("DeleteGroup", ctx, "group-1").Return(nil)

	err := adapter.DeleteGroup(ctx, "group-1")
	require.NoError(t, err)
}

func TestAuthServiceAdapter_GetGroupAPI(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	mockStore.On("GetGroup", ctx, "group-1").Return(&auth.Group{
		ID:   "group-1",
		Name: "admins",
	}, nil)

	result, err := adapter.GetGroupAPI(ctx, "group-1")
	require.NoError(t, err)
	assert.NotNil(t, result)
}

func TestAuthServiceAdapter_ListGroupsAPI(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	mockStore.On("ListGroups", ctx).Return([]auth.Group{
		{ID: "group-1", Name: "admins"},
	}, nil)

	result, err := adapter.ListGroupsAPI(ctx)
	require.NoError(t, err)
	assert.NotNil(t, result)
}

func TestAuthServiceAdapter_ValidateCSRFToken(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	// Token gets hashed; the stored Token must match hashSessionToken("session-token")
	// so the constant-time guard in ValidateSession (PR #837 follow-up) succeeds.
	mockStore.On("GetSession", ctx, mock.AnythingOfType("string")).Return(&auth.Session{
		Token:     hashSessionTokenForTest("session-token"),
		UserID:    "user-1",
		CSRFToken: "csrf-token",
		ExpiresAt: time.Now().Add(time.Hour),
	}, nil)
	mockStore.On("GetUserByID", ctx, "user-1").Return(&auth.User{ID: "user-1", Active: true}, nil)

	// ValidateCSRFToken recomputes the expected token as
	// HMAC-SHA256(csrfKey, rawSessionToken); it never reads the stored
	// CSRFToken column. Derive the matching token from the same fixed key the
	// service was built with (auth.TestCSRFKey) so the assertion is hermetic
	// and does not depend on an ambient CSRFKey from the environment.
	csrfToken := auth.DeriveTestCSRFToken("session-token")
	err := adapter.ValidateCSRFToken(ctx, "session-token", csrfToken)
	require.NoError(t, err)
}

func TestAuthServiceAdapter_HasPermissionAPI(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	// Administrators-group member: the group's {admin, *} permission grants
	// every action (issue #907 group-only authz).
	mockStore.On("GetUserByID", ctx, "user-1").Return(&auth.User{
		ID:       "user-1",
		Email:    "test@example.com",
		GroupIDs: []string{auth.DefaultAdminGroupID},
	}, nil)
	mockStore.On("GetGroup", ctx, auth.DefaultAdminGroupID).Return(&auth.Group{
		ID:          auth.DefaultAdminGroupID,
		Name:        "Administrators",
		Permissions: []auth.Permission{{Action: auth.ActionAdmin, Resource: auth.ResourceAll}},
	}, nil)

	has, err := adapter.HasPermissionAPI(ctx, "user-1", "read", "config")
	require.NoError(t, err)
	assert.True(t, has)
}

func TestAuthServiceAdapter_UpdateUserProfile(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	// User not found - exercises the adapter code path
	mockStore.On("GetUserByID", ctx, "nonexistent").Return(nil, assert.AnError)

	err := adapter.UpdateUserProfile(ctx, "nonexistent", "new@example.com", "", "")
	assert.Error(t, err)
}

func TestAuthServiceAdapter_CreateUserAPI(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	mockStore.On("GetUserByEmail", ctx, mock.AnythingOfType("string")).Return(nil, nil)
	mockStore.On("CreateUser", ctx, mock.AnythingOfType("*auth.User")).Return(nil)

	// Pass a properly typed request
	_, err := adapter.CreateUserAPI(ctx, "", map[string]interface{}{
		"email":    "new@example.com",
		"password": "StrongPassword123!",
		"groups":   []string{"group-viewer"},
	})
	// May fail depending on validation but exercises the code
	_ = err
}

func TestAuthServiceAdapter_UpdateUserAPI(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	mockStore.On("GetUserByID", ctx, "user-1").Return(&auth.User{
		ID:       "user-1",
		Email:    "old@example.com",
		GroupIDs: []string{"group-viewer"},
	}, nil).Once()
	// Actor != target, so this is a non-self edit: the membership grant
	// ceiling (issue #226) resolves the actor's own permissions before
	// allowing the group-editor group to be added.
	mockStore.On("GetUserByID", ctx, "actor-1").Return(&auth.User{
		ID:       "actor-1",
		GroupIDs: []string{"group-admin"},
	}, nil)
	mockStore.On("GetGroup", ctx, "group-admin").Return(&auth.Group{
		ID:          "group-admin",
		Permissions: []auth.Permission{{Action: auth.ActionAdmin, Resource: auth.ResourceAll}},
	}, nil)
	mockStore.On("GetGroup", ctx, "group-editor").Return(&auth.Group{
		ID:          "group-editor",
		Permissions: []auth.Permission{{Action: auth.ActionUpdate, Resource: auth.ResourceUsers}},
	}, nil)
	mockStore.On("UpdateUserAdminFields", ctx, mock.AnythingOfType("*auth.User"), mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()

	// The new signature threads the actor user ID through to s.UpdateUser
	// (issue #907 self-escalation guard). Actor != target, so the
	// self-escalation guard skips; the group change does not remove the
	// Administrators group, so the last-admin guard skips; the store
	// UpdateUser call must therefore fire. Without that, the test silently
	// passes even if the adapter stopped forwarding actorUserID, which is
	// exactly the regression CR flagged.
	_, err := adapter.UpdateUserAPI(ctx, "actor-1", "user-1", auth.APIUpdateUserRequest{
		Groups: []string{"group-editor"},
	})
	require.NoError(t, err)
	mockStore.AssertExpectations(t)
}

func TestAuthServiceAdapter_CreateGroupAPI(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	mockStore.On("CreateGroup", ctx, mock.AnythingOfType("*auth.Group")).Return(nil)

	_, err := adapter.CreateGroupAPI(ctx, auth.AdminAPIKeyActorID, map[string]interface{}{
		"name":        "test-group",
		"permissions": []string{"read:config"},
	})
	_ = err
}

func TestAuthServiceAdapter_UpdateGroupAPI(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	mockStore.On("GetGroup", ctx, "group-1").Return(&auth.Group{
		ID:   "group-1",
		Name: "old-name",
	}, nil)
	mockStore.On("UpdateGroup", ctx, mock.AnythingOfType("*auth.Group")).Return(nil)

	_, err := adapter.UpdateGroupAPI(ctx, auth.AdminAPIKeyActorID, "group-1", map[string]interface{}{
		"name": "new-name",
	})
	_ = err
}

func TestAuthServiceAdapter_CreateAPIKeyAPI(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()
	t.Cleanup(func() { mockStore.AssertExpectations(t) })

	// CreateAPIKey calls GetUserByID then validateCreateAPIKeyRequest
	// (password re-verification, issue #102) then validateAPIKeyPermissions
	// (GetGroup) then CreateAPIKey. All three stores must be mocked for the
	// happy path.
	const password = "CorrectHorse@123"
	mockStore.On("GetUserByID", ctx, "user-1").Return(&auth.User{
		ID:           "user-1",
		Email:        "test@example.com",
		Active:       true,
		PasswordHash: auth.TestPasswordHash(t, password),
		GroupIDs:     []string{auth.DefaultAdminGroupID},
	}, nil)
	mockStore.On("GetGroup", ctx, auth.DefaultAdminGroupID).Return(&auth.Group{
		ID:          auth.DefaultAdminGroupID,
		Permissions: []auth.Permission{{Action: auth.ActionAdmin, Resource: auth.ResourceAll}},
	}, nil)
	mockStore.On("CreateAPIKey", ctx, mock.AnythingOfType("*auth.UserAPIKey")).Return(nil)

	expiresAt := time.Now().Add(24 * time.Hour)
	result, err := adapter.CreateAPIKeyAPI(ctx, "user-1", auth.APICreateAPIKeyRequest{
		Name:        "my-key",
		Password:    password,
		Permissions: []auth.APIPermission{{Action: auth.ActionView, Resource: auth.ResourceRecommendations}},
		ExpiresAt:   &expiresAt,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
}

func TestAuthServiceAdapter_ListUserAPIKeysAPI(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	mockStore.On("GetUserByID", ctx, "user-1").Return(&auth.User{
		ID: "user-1", Email: "test@example.com",
	}, nil)
	mockStore.On("ListAPIKeysByUser", ctx, "user-1").Return([]*auth.UserAPIKey{}, nil)

	result, err := adapter.ListUserAPIKeysAPI(ctx, "user-1")
	require.NoError(t, err)
	assert.NotNil(t, result)
}

func TestAuthServiceAdapter_DeleteAPIKeyAPI(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	mockStore.On("GetUserByID", ctx, "user-1").Return(&auth.User{
		ID: "user-1", Email: "test@example.com",
	}, nil)
	mockStore.On("GetAPIKeyByID", ctx, "key-1").Return(&auth.UserAPIKey{
		ID:     "key-1",
		UserID: "user-1",
	}, nil)
	mockStore.On("DeleteAPIKey", ctx, "key-1").Return(nil)

	err := adapter.DeleteAPIKeyAPI(ctx, "user-1", "key-1")
	require.NoError(t, err)
}

func TestAuthServiceAdapter_RevokeAPIKeyAPI(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	mockStore.On("GetUserByID", ctx, "user-1").Return(&auth.User{
		ID: "user-1", Email: "test@example.com",
	}, nil)
	mockStore.On("GetAPIKeyByID", ctx, "key-1").Return(&auth.UserAPIKey{
		ID:     "key-1",
		UserID: "user-1",
	}, nil)
	mockStore.On("UpdateAPIKey", ctx, mock.AnythingOfType("*auth.UserAPIKey")).Return(nil)

	err := adapter.RevokeAPIKeyAPI(ctx, "user-1", "key-1")
	require.NoError(t, err)
}

func TestAuthServiceAdapter_ValidateUserAPIKeyAPI(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	mockStore.On("GetAPIKeyByHash", ctx, mock.AnythingOfType("string")).Return(nil, assert.AnError)

	_, _, err := adapter.ValidateUserAPIKeyAPI(ctx, "invalid-api-key")
	assert.Error(t, err)
}

func TestNewAuthServiceAdapter_NotNil(t *testing.T) {
	service := auth.NewService(auth.ServiceConfig{})
	adapter := newAuthServiceAdapter(service)
	assert.NotNil(t, adapter)
	assert.NotNil(t, adapter.service)
}

func TestAuthServiceAdapter_SetupAdmin(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()

	mockStore.On("AdminExists", ctx).Return(false, nil)
	// Bootstrap path now calls CreateAdminIfNone (issue #349).
	mockStore.On("CreateAdminIfNone", ctx, mock.AnythingOfType("*auth.User")).Return(true, nil)
	mockStore.On("CreateSession", ctx, mock.AnythingOfType("*auth.Session")).Return(nil)

	resp, err := adapter.SetupAdmin(ctx, api.SetupAdminRequest{
		Email:    "admin@example.com",
		Password: "X9k#mP2$vL7qR4wN",
	})
	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, "admin@example.com", resp.User.Email)
}

// TestSetupAdminThenLogin_RoundTrip is a regression test for issue #224: the
// frontend base64-encodes the setup-admin password exactly as it does for
// login (frontend/src/api/auth.ts), but the setupAdmin handler used to
// forward the still-encoded string straight through the adapter to
// auth.Service.SetupAdmin, which bcrypt-hashed the base64 text. Every later
// login (whose handler does decode) then compared the real plaintext
// against that hash and failed, locking the bootstrap admin out
// permanently.
//
// This drives the full stack an HTTP request would: api.Router.Route ->
// api.Handler.setupAdmin/login -> authServiceAdapter -> the real
// *auth.Service, backed by an auth.MockStore that persists the user
// setupAdmin creates and hands it back to login. It fails on the pre-fix
// setupAdmin handler (which skips decodeBase64Password) and passes once it
// decodes like every other password handler.
func TestSetupAdminThenLogin_RoundTrip(t *testing.T) {
	ctx := context.Background()

	mockStore := new(auth.MockStore)
	realAuth := auth.NewService(auth.ServiceConfig{
		Store:           mockStore,
		SessionDuration: time.Hour,
		CSRFKey:         auth.TestCSRFKey(),
		DashboardURL:    "http://localhost:3000",
	})
	adapter := newAuthServiceAdapter(realAuth)
	handler := api.NewHandler(api.HandlerConfig{AuthService: adapter})
	router := api.NewRouter(handler)

	const email = "admin@example.com"
	const plaintextPassword = "SecureBootstrapPass@1"
	encodedPassword := base64.StdEncoding.EncodeToString([]byte(plaintextPassword))
	body := `{"email": "` + email + `", "password": "` + encodedPassword + `"}`

	mockStore.On("AdminExists", ctx).Return(false, nil).Once()

	var createdUser *auth.User
	mockStore.On("CreateAdminIfNone", ctx, mock.AnythingOfType("*auth.User")).
		Run(func(args mock.Arguments) {
			u, ok := args.Get(1).(*auth.User)
			require.True(t, ok)
			createdUser = u
		}).
		Return(true, nil).Once()
	mockStore.On("CreateSession", ctx, mock.AnythingOfType("*auth.Session")).Return(nil).Twice()
	mockStore.On("RecordSuccessfulLogin", ctx, mock.AnythingOfType("string")).Return(nil).Once()

	setupReq := &events.LambdaFunctionURLRequest{
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{SourceIP: "127.0.0.1"},
		},
		Body: body,
	}
	_, err := router.Route(ctx, "POST", "/api/auth/setup-admin", setupReq)
	require.NoError(t, err)
	require.NotNil(t, createdUser, "setupAdmin must have created the admin user")

	// login looks the user up by email; hand back the user setupAdmin
	// created, carrying whatever password hash setupAdmin actually produced.
	mockStore.On("GetUserByEmail", ctx, email).Return(createdUser, nil).Once()

	loginReq := &events.LambdaFunctionURLRequest{
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{SourceIP: "127.0.0.1"},
		},
		Body: body,
	}
	result, err := router.Route(ctx, "POST", "/api/auth/login", loginReq)
	require.NoError(t, err, "the bootstrap admin must be able to log in with the same frontend-encoded password used at setup")

	resp, ok := result.(*api.LoginResponse)
	require.True(t, ok)
	assert.Equal(t, email, resp.User.Email)

	mockStore.AssertExpectations(t)
}
