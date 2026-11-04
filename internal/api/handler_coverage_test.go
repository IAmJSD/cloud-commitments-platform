package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Tests for buildResponse error handling

func TestHandler_buildResponse_WithError(t *testing.T) {
	handler := &Handler{}
	headers := map[string]string{"Content-Type": "application/json"}

	// buildResponse never returns an error; when called with a non-nil err arg
	// it converts it to a 500 response body so callers can use single-return form.
	resp := handler.buildResponse(200, headers, nil, errors.New("test error"))

	assert.Equal(t, 500, resp.StatusCode)
	assert.Contains(t, resp.Body, "internal server error")
}

func TestHandler_buildResponse_MarshalError(t *testing.T) {
	handler := &Handler{}
	headers := map[string]string{"Content-Type": "application/json"}

	// Create an unmarshalable value (channel)
	type badType struct {
		Ch chan int `json:"ch"`
	}
	badValue := badType{Ch: make(chan int)}

	resp := handler.buildResponse(200, headers, badValue, nil)

	assert.Equal(t, 500, resp.StatusCode)
	assert.Contains(t, resp.Body, "internal server error")
}

func TestHandler_buildResponse_NilBody(t *testing.T) {
	handler := &Handler{}
	headers := map[string]string{"Content-Type": "application/json"}

	resp := handler.buildResponse(200, headers, nil, nil)

	assert.Equal(t, 200, resp.StatusCode)
	// Q1 (Phase-2 UX plan): nil-body success serializes as "{}" so the
	// frontend's response.json() doesn't throw SyntaxError on DELETE and
	// other empty-response paths.
	assert.Equal(t, "{}", resp.Body)
}

// Tests for validateSecurityContext

func TestHandler_validateSecurityContext_PublicEndpoint(t *testing.T) {
	ctx := context.Background()
	handler := &Handler{}

	req := &events.LambdaFunctionURLRequest{}
	headers := map[string]string{}

	// Public endpoint should return nil
	_, resp := handler.validateSecurityContext(ctx, req, "GET", "/api/health", headers)
	assert.Nil(t, resp)
}

func TestHandler_validateSecurityContext_Unauthorized(t *testing.T) {
	ctx := context.Background()
	handler := &Handler{apiKey: "secret-key"}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{},
	}
	headers := map[string]string{}

	_, resp := handler.validateSecurityContext(ctx, req, "GET", "/api/config", headers)
	assert.NotNil(t, resp)
	assert.Equal(t, 401, resp.StatusCode)
}

func TestHandler_validateSecurityContext_CSRFFailure(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	// Setup authentication to pass (via bearer token), then CSRF to fail
	mockAuth.On("ValidateSession", mock.Anything, "test-token").Return(&Session{UserID: "user-1"}, nil)
	mockAuth.On("ValidateCSRFToken", mock.Anything, mock.Anything, mock.Anything).Return(errors.New("csrf failed"))

	// Use bearer token auth (not API key) to trigger CSRF validation path
	handler := &Handler{auth: mockAuth}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{
			"Authorization": "Bearer test-token",
		},
	}
	headers := map[string]string{}

	// PUT on /api/config requires CSRF
	_, resp := handler.validateSecurityContext(ctx, req, "PUT", "/api/config", headers)
	require.NotNil(t, resp)
	assert.Equal(t, 403, resp.StatusCode)
}

// Tests for request validation

func TestHandler_validateRequestContext_BodyTooLarge(t *testing.T) {
	ctx := context.Background()
	handler := &Handler{}

	// Create a large body (over 1MB)
	largeBody := make([]byte, 1024*1024+1)
	req := &events.LambdaFunctionURLRequest{
		Body: string(largeBody),
	}
	headers := map[string]string{}

	_, resp := handler.validateRequestContext(ctx, req, "POST", "/api/config", headers)
	assert.NotNil(t, resp)
	assert.Equal(t, 413, resp.StatusCode)
}

func TestHandler_validateRequestContext_InvalidContentType(t *testing.T) {
	ctx := context.Background()
	handler := &Handler{}

	// Content-Type validation happens before security checks
	// For a POST request with body and wrong content-type, it should return 415
	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{
			"Content-Type": "text/plain",
		},
		Body: "some body content",
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{
				Method: "POST",
			},
		},
	}
	headers := map[string]string{}

	_, resp := handler.validateRequestContext(ctx, req, "POST", "/api/health", headers)
	require.NotNil(t, resp)
	assert.Equal(t, 415, resp.StatusCode)
}

// Tests for setSecurityHeaders - additional coverage

func TestSetSecurityHeaders_AllHeaders(t *testing.T) {
	headers := make(map[string]string)
	result := setSecurityHeaders(headers)

	assert.Contains(t, result["Content-Security-Policy"], "default-src 'none'")
	assert.Contains(t, result["Strict-Transport-Security"], "max-age=")
	assert.Equal(t, "nosniff", result["X-Content-Type-Options"])
	assert.Equal(t, "DENY", result["X-Frame-Options"])
	// X-XSS-Protection has been removed: deprecated and no-op in modern browsers
	assert.Empty(t, result["X-XSS-Protection"])
	assert.Contains(t, result["Referrer-Policy"], "strict-origin")
	assert.Contains(t, result["Permissions-Policy"], "geolocation=()")
	assert.Contains(t, result["Cache-Control"], "no-store")
}

// Tests for router handlers that are wrappers.
//
// Each subtest pins two things (issue #116):
//   - the collaborator call: mocks come from strictAuthMock (or register an
//     equivalent t.Cleanup), so an expectation the handler stops consuming
//     fails the subtest, and expectations match the exact forwarded
//     arguments rather than mock.Anything where the handler builds them;
//   - the response: the returned value is compared with what the handler must
//     produce, not merely checked for non-nil. Fixtures are built by a func so
//     the expected value is a fresh copy: comparing against the very pointer
//     or map handed to the mock would also pass if the handler mutated it in
//     place (say, cleared LoginResponse.Token).
//
// grantAdmin registrations are .Maybe() by design: they model the caller's
// permission state for handlers that consult it, not a call the handler is
// required to make, so AssertExpectations does not demand them. Session-only
// handlers (getCurrentUser, updateProfile, changePassword) consult no
// permission and do not call grantAdmin.

// strictAuthMock returns a MockAuthService whose non-.Maybe() expectations
// must all have been consumed when the subtest ends.
func strictAuthMock(t *testing.T) *MockAuthService {
	t.Helper()
	m := new(MockAuthService)
	t.Cleanup(func() { m.AssertExpectations(t) })
	return m
}

func TestRouter_Handlers_Coverage(t *testing.T) {
	ctx := context.Background()
	const testID = "11111111-1111-1111-1111-111111111111"

	t.Run("loginHandler", func(t *testing.T) {
		mockAuth := strictAuthMock(t)
		want := func() *LoginResponse {
			return &LoginResponse{
				Token:     "tok",
				ExpiresAt: "2026-01-01T00:00:00Z",
				User:      &UserInfo{ID: "user-1", Email: "test@example.com"},
				CSRFToken: "csrf-tok",
			}
		}
		// The handler must forward the email and the DECODED password.
		mockAuth.On("Login", ctx, LoginRequest{Email: "test@example.com", Password: "pass123"}).Return(want(), nil)

		h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}
		router := NewRouter(h)

		req := &events.LambdaFunctionURLRequest{
			Body: `{"email": "test@example.com", "password": "` + b64("pass123") + `"}`,
		}

		result, err := router.loginHandler(ctx, req, nil)
		require.NoError(t, err)
		assert.Equal(t, want(), result)
	})

	t.Run("logoutHandler", func(t *testing.T) {
		mockAuth := strictAuthMock(t)
		// Required: a logout that skips auth.Logout leaves the session live.
		mockAuth.On("Logout", ctx, "test-token").Return(nil)

		h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}
		router := NewRouter(h)

		req := &events.LambdaFunctionURLRequest{
			Headers: map[string]string{"Authorization": "Bearer test-token"},
		}

		result, err := router.logoutHandler(ctx, req, nil)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"status": "logged out"}, result)
	})

	t.Run("getCurrentUserHandler", func(t *testing.T) {
		mockAuth := strictAuthMock(t)
		mockAuth.On("ValidateSession", ctx, "test-token").Return(&Session{UserID: "user-1"}, nil)
		mockAuth.On("GetUser", ctx, "user-1").Return(&User{
			ID:         "user-1",
			Email:      "test@example.com",
			Groups:     []string{"group-1"},
			MFAEnabled: true,
		}, nil)

		h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}
		router := NewRouter(h)

		req := &events.LambdaFunctionURLRequest{
			Headers: map[string]string{"Authorization": "Bearer test-token"},
		}

		result, err := router.getCurrentUserHandler(ctx, req, nil)
		require.NoError(t, err)
		assert.Equal(t, &CurrentUserResponse{
			ID:         "user-1",
			Email:      "test@example.com",
			Groups:     []string{"group-1"},
			MFAEnabled: true,
		}, result)
	})

	t.Run("checkAdminExistsHandler", func(t *testing.T) {
		mockAuth := strictAuthMock(t)
		mockAuth.On("CheckAdminExists", ctx).Return(true, nil)

		h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}
		router := NewRouter(h)

		result, err := router.checkAdminExistsHandler(ctx, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, &AdminExistsResponse{AdminExists: true}, result)
	})

	t.Run("setupAdminHandler", func(t *testing.T) {
		mockAuth := strictAuthMock(t)
		want := func() *LoginResponse {
			return &LoginResponse{Token: "admin-tok", User: &UserInfo{ID: "admin-1", Email: "admin@example.com"}}
		}
		// Password must be base64-encoded, exactly as the frontend encodes it
		// (issue #224) - setupAdmin decodes it before forwarding to auth.
		mockAuth.On("SetupAdmin", ctx, SetupAdminRequest{Email: "admin@example.com", Password: "pass123"}).Return(want(), nil)

		h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}
		router := NewRouter(h)

		req := &events.LambdaFunctionURLRequest{
			Body: `{"email": "admin@example.com", "password": "` + b64("pass123") + `"}`,
		}

		result, err := router.setupAdminHandler(ctx, req, nil)
		require.NoError(t, err)
		assert.Equal(t, want(), result)
	})

	t.Run("forgotPasswordHandler", func(t *testing.T) {
		mockAuth := strictAuthMock(t)
		// The response is identical whether or not a reset was requested (no
		// email enumeration), so the expectation is the only thing proving the
		// handler actually asked for one.
		mockAuth.On("RequestPasswordReset", ctx, "test@example.com").Return(nil)

		h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}
		router := NewRouter(h)

		req := &events.LambdaFunctionURLRequest{
			Body: `{"email": "test@example.com"}`,
		}

		result, err := router.forgotPasswordHandler(ctx, req, nil)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"status": "if the email exists, a reset link has been sent"}, result)
	})

	t.Run("resetPasswordHandler", func(t *testing.T) {
		mockAuth := strictAuthMock(t)
		// The token and the DECODED new password must reach the service (#356).
		mockAuth.On("ConfirmPasswordReset", ctx, PasswordResetConfirm{Token: "reset-token", NewPassword: "newpass"}).Return(nil)

		h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}
		router := NewRouter(h)

		req := &events.LambdaFunctionURLRequest{
			Body: `{"token": "reset-token", "new_password": "` + b64("newpass") + `"}`,
		}

		result, err := router.resetPasswordHandler(ctx, req, nil)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"status": "password reset successful"}, result)
	})

	t.Run("updateProfileHandler", func(t *testing.T) {
		mockAuth := strictAuthMock(t)
		mockAuth.On("ValidateSession", ctx, "test-token").Return(&Session{UserID: "user-1"}, nil)
		mockAuth.On("UpdateUserProfile", ctx, "user-1", "newemail@example.com", "", "").Return(nil)

		h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}
		router := NewRouter(h)

		req := &events.LambdaFunctionURLRequest{
			Headers: map[string]string{"Authorization": "Bearer test-token"},
			Body:    `{"email": "newemail@example.com"}`,
		}

		result, err := router.updateProfileHandler(ctx, req, nil)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"status": "profile updated"}, result)
	})

	t.Run("changePasswordHandler", func(t *testing.T) {
		mockAuth := strictAuthMock(t)
		mockAuth.On("ValidateSession", ctx, "test-token").Return(&Session{UserID: "user-1"}, nil)
		mockAuth.On("ChangePasswordAPI", ctx, "user-1", "oldpass", "newpass").Return(nil)

		h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}
		router := NewRouter(h)

		req := &events.LambdaFunctionURLRequest{
			Headers: map[string]string{"Authorization": "Bearer test-token"},
			Body:    `{"current_password": "` + b64("oldpass") + `", "new_password": "` + b64("newpass") + `"}`,
		}

		result, err := router.changePasswordHandler(ctx, req, nil)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"status": "password changed"}, result)
	})

	t.Run("listAPIKeysHandler", func(t *testing.T) {
		mockAuth := strictAuthMock(t)
		mockAuth.On("ValidateSession", ctx, "test-token").Return(&Session{UserID: "user-1"}, nil)
		mockAuth.grantAdmin()
		keys := func() []interface{} { return []interface{}{map[string]string{"id": "key-1", "name": "My Key"}} }
		mockAuth.On("ListUserAPIKeysAPI", ctx, "user-1").Return(keys(), nil)

		h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}
		router := NewRouter(h)

		req := &events.LambdaFunctionURLRequest{
			Headers: map[string]string{"Authorization": "Bearer test-token"},
		}

		result, err := router.listAPIKeysHandler(ctx, req, nil)
		require.NoError(t, err)
		assert.Equal(t, keys(), result)
	})

	t.Run("createAPIKeyHandler", func(t *testing.T) {
		mockAuth := strictAuthMock(t)
		mockAuth.On("ValidateSession", ctx, "test-token").Return(&Session{UserID: "user-1"}, nil)
		mockAuth.grantAdmin()
		created := func() map[string]string { return map[string]string{"key_id": "key-1"} }
		mockAuth.On("CreateAPIKeyAPI", ctx, "user-1", auth.APICreateAPIKeyRequest{Name: "My Key", Password: "pass123"}).Return(created(), nil)

		h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}
		router := NewRouter(h)

		req := &events.LambdaFunctionURLRequest{
			Headers: map[string]string{"Authorization": "Bearer test-token"},
			Body:    `{"name": "My Key", "password": "` + b64("pass123") + `"}`,
		}

		result, err := router.createAPIKeyHandler(ctx, req, nil)
		require.NoError(t, err)
		assert.Equal(t, created(), result)
	})

	t.Run("deleteAPIKeyHandler", func(t *testing.T) {
		mockAuth := strictAuthMock(t)
		mockAuth.On("ValidateSession", ctx, "test-token").Return(&Session{UserID: "user-1"}, nil)
		mockAuth.grantAdmin()
		mockAuth.On("DeleteAPIKeyAPI", ctx, "user-1", testID).Return(nil)

		h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}
		router := NewRouter(h)

		req := &events.LambdaFunctionURLRequest{
			Headers: map[string]string{"Authorization": "Bearer test-token"},
			RequestContext: events.LambdaFunctionURLRequestContext{
				HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{
					Path: "/api/api-keys/" + testID,
				},
			},
		}

		result, err := router.deleteAPIKeyHandler(ctx, req, nil)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"status": "deleted"}, result)
	})

	t.Run("revokeAPIKeyHandler", func(t *testing.T) {
		mockAuth := strictAuthMock(t)
		mockAuth.On("ValidateSession", ctx, "test-token").Return(&Session{UserID: "user-1"}, nil)
		mockAuth.grantAdmin()
		mockAuth.On("RevokeAPIKeyAPI", ctx, "user-1", testID).Return(nil)

		h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}
		router := NewRouter(h)

		req := &events.LambdaFunctionURLRequest{
			Headers: map[string]string{"Authorization": "Bearer test-token"},
			RequestContext: events.LambdaFunctionURLRequestContext{
				HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{
					Path: "/api/api-keys/" + testID + "/revoke",
				},
			},
		}

		result, err := router.revokeAPIKeyHandler(ctx, req, nil)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"status": "revoked"}, result)
	})

	t.Run("listUsersHandler", func(t *testing.T) {
		mockAuth := strictAuthMock(t)
		mockAuth.On("ValidateSession", ctx, "admin-token").Return(&Session{UserID: "admin"}, nil)
		mockAuth.grantAdmin()
		users := func() []interface{} { return []interface{}{map[string]string{"id": "user-1"}} }
		mockAuth.On("ListUsersAPI", ctx).Return(users(), nil)

		h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}
		router := NewRouter(h)

		req := &events.LambdaFunctionURLRequest{
			Headers: map[string]string{"Authorization": "Bearer admin-token"},
		}

		result, err := router.listUsersHandler(ctx, req, nil)
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"users": users()}, result)
	})

	t.Run("createUserHandler", func(t *testing.T) {
		mockAuth := strictAuthMock(t)
		mockAuth.On("ValidateSession", ctx, "admin-token").Return(&Session{UserID: "admin"}, nil)
		mockAuth.grantAdmin()
		const groupID = "00000000-0000-5000-8000-000000000005"
		created := func() *auth.APICreateUserResponse {
			return &auth.APICreateUserResponse{APIUser: &auth.APIUser{ID: "new-user"}}
		}
		// The actor is the session user (grant ceiling, #226) and the password
		// arrives decoded.
		mockAuth.On("CreateUserAPI", ctx, "admin", auth.APICreateUserRequest{
			Email:    "newuser@example.com",
			Password: "pass123",
			Groups:   []string{groupID},
		}).Return(created(), nil)

		h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}
		router := NewRouter(h)

		req := &events.LambdaFunctionURLRequest{
			Headers: map[string]string{"Authorization": "Bearer admin-token"},
			Body:    `{"email": "newuser@example.com", "password": "` + b64("pass123") + `", "groups": ["` + groupID + `"]}`,
		}

		result, err := router.createUserHandler(ctx, req, nil)
		require.NoError(t, err)
		assert.Equal(t, created(), result)
	})

	t.Run("getUserHandler", func(t *testing.T) {
		mockAuth := strictAuthMock(t)
		mockAuth.On("ValidateSession", ctx, "admin-token").Return(&Session{UserID: "admin"}, nil)
		mockAuth.grantAdmin()
		user := func() *User { return &User{ID: testID, Email: "user@example.com"} }
		mockAuth.On("GetUser", ctx, testID).Return(user(), nil)

		h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}
		router := NewRouter(h)

		req := &events.LambdaFunctionURLRequest{
			Headers: map[string]string{"Authorization": "Bearer admin-token"},
		}
		params := map[string]string{"id": testID}

		result, err := router.getUserHandler(ctx, req, params)
		require.NoError(t, err)
		assert.Equal(t, user(), result)
	})

	t.Run("updateUserHandler", func(t *testing.T) {
		mockAuth := strictAuthMock(t)
		mockAuth.On("ValidateSession", ctx, "admin-token").Return(&Session{UserID: "admin"}, nil)
		mockAuth.grantAdmin()
		updated := func() *auth.APIUser { return &auth.APIUser{ID: testID, Email: "updated@example.com"} }
		mockAuth.On("UpdateUserAPI", ctx, "admin", testID, auth.APIUpdateUserRequest{Email: "updated@example.com"}).Return(updated(), nil)

		h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}
		router := NewRouter(h)

		req := &events.LambdaFunctionURLRequest{
			Headers: map[string]string{"Authorization": "Bearer admin-token"},
			Body:    `{"email": "updated@example.com"}`,
		}
		params := map[string]string{"id": testID}

		result, err := router.updateUserHandler(ctx, req, params)
		require.NoError(t, err)
		assert.Equal(t, updated(), result)
	})

	t.Run("listGroupsHandler", func(t *testing.T) {
		mockAuth := strictAuthMock(t)
		mockAuth.On("ValidateSession", ctx, "admin-token").Return(&Session{UserID: "admin"}, nil)
		mockAuth.grantAdmin()
		groups := func() []interface{} { return []interface{}{map[string]string{"id": "group-1"}} }
		mockAuth.On("ListGroupsAPI", ctx).Return(groups(), nil)

		h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}
		router := NewRouter(h)

		req := &events.LambdaFunctionURLRequest{
			Headers: map[string]string{"Authorization": "Bearer admin-token"},
		}

		result, err := router.listGroupsHandler(ctx, req, nil)
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"groups": groups()}, result)
	})

	t.Run("createGroupHandler", func(t *testing.T) {
		mockAuth := strictAuthMock(t)
		mockAuth.On("ValidateSession", ctx, "admin-token").Return(&Session{UserID: "admin"}, nil)
		mockAuth.grantAdmin()
		created := func() *auth.APIGroup { return &auth.APIGroup{ID: "new-group"} }
		mockAuth.On("CreateGroupAPI", ctx, "admin", auth.APICreateGroupRequest{Name: "New Group"}).Return(created(), nil)

		h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}
		router := NewRouter(h)

		req := &events.LambdaFunctionURLRequest{
			Headers: map[string]string{"Authorization": "Bearer admin-token"},
			Body:    `{"name": "New Group"}`,
		}

		result, err := router.createGroupHandler(ctx, req, nil)
		require.NoError(t, err)
		assert.Equal(t, created(), result)
	})

	t.Run("getGroupHandler", func(t *testing.T) {
		mockAuth := strictAuthMock(t)
		mockAuth.On("ValidateSession", ctx, "admin-token").Return(&Session{UserID: "admin"}, nil)
		mockAuth.grantAdmin()
		group := func() map[string]interface{} { return map[string]interface{}{"id": testID, "name": "Group"} }
		mockAuth.On("GetGroupAPI", ctx, testID).Return(group(), nil)

		h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}
		router := NewRouter(h)

		req := &events.LambdaFunctionURLRequest{
			Headers: map[string]string{"Authorization": "Bearer admin-token"},
		}
		params := map[string]string{"id": testID}

		result, err := router.getGroupHandler(ctx, req, params)
		require.NoError(t, err)
		assert.Equal(t, group(), result)
	})

	t.Run("updateGroupHandler", func(t *testing.T) {
		mockAuth := strictAuthMock(t)
		mockAuth.On("ValidateSession", ctx, "admin-token").Return(&Session{UserID: "admin"}, nil)
		mockAuth.grantAdmin()
		updated := func() *auth.APIGroup { return &auth.APIGroup{ID: testID, Name: "Updated Group"} }
		mockAuth.On("UpdateGroupAPI", ctx, "admin", testID, auth.APIUpdateGroupRequest{Name: "Updated Group"}).Return(updated(), nil)

		h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}
		router := NewRouter(h)

		req := &events.LambdaFunctionURLRequest{
			Headers: map[string]string{"Authorization": "Bearer admin-token"},
			Body:    `{"name": "Updated Group"}`,
		}
		params := map[string]string{"id": testID}

		result, err := router.updateGroupHandler(ctx, req, params)
		require.NoError(t, err)
		assert.Equal(t, updated(), result)
	})

	t.Run("deleteGroupHandler", func(t *testing.T) {
		mockAuth := strictAuthMock(t)
		mockAuth.On("ValidateSession", ctx, "admin-token").Return(&Session{UserID: "admin"}, nil)
		mockAuth.grantAdmin()
		mockAuth.On("DeleteGroup", ctx, testID).Return(nil)

		h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}
		router := NewRouter(h)

		req := &events.LambdaFunctionURLRequest{
			Headers: map[string]string{"Authorization": "Bearer admin-token"},
		}
		params := map[string]string{"id": testID}

		result, err := router.deleteGroupHandler(ctx, req, params)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"status": "group deleted"}, result)
	})

	t.Run("getPublicInfoHandler", func(t *testing.T) {
		mockAuth := strictAuthMock(t)
		mockAuth.On("CheckAdminExists", ctx).Return(true, nil)

		h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}
		router := NewRouter(h)

		req := &events.LambdaFunctionURLRequest{
			RequestContext: events.LambdaFunctionURLRequestContext{
				HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{
					SourceIP: "192.168.1.1",
				},
			},
		}

		result, err := router.getPublicInfoHandler(ctx, req, nil)
		require.NoError(t, err)
		assert.Equal(t, &PublicInfoResponse{Version: "1.0.0", AdminExists: true}, result)
	})

	t.Run("getHistoryAnalyticsHandler", func(t *testing.T) {
		mockAuth, req := adminAnalyticsReq(ctx)
		t.Cleanup(func() { mockAuth.AssertExpectations(t) })
		mockClient := new(MockAnalyticsClient)
		t.Cleanup(func() { mockClient.AssertExpectations(t) })
		points := func() []HistoryDataPoint {
			return []HistoryDataPoint{{
				Timestamp:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				TotalSavings:  100,
				PurchaseCount: 2,
			}}
		}
		summary := func() *HistorySummary { return &HistorySummary{TotalPurchases: 2, TotalCompleted: 2} }
		// No account_id: an unrestricted caller aggregates every account (nil
		// filters), no provider filter, and the default "hourly" interval.
		mockClient.On("QueryHistory", ctx, []string(nil), map[string][]string(nil), "", mock.Anything, mock.Anything, "hourly").
			Return(points(), summary(), nil)

		h := &Handler{auth: mockAuth, analyticsClient: mockClient}
		router := NewRouter(h)

		result, err := router.getHistoryAnalyticsHandler(ctx, req, map[string]string{})
		require.NoError(t, err)
		resp, ok := result.(*AnalyticsResponse)
		require.True(t, ok, "result type %T", result)
		assert.Equal(t, "hourly", resp.Interval)
		assert.Equal(t, points(), resp.DataPoints)
		assert.Equal(t, summary(), resp.Summary)
		// The reported window is the one that was queried.
		require.Len(t, mockClient.Calls, 1)
		args := mockClient.Calls[0].Arguments
		assert.Equal(t, args.Get(4).(time.Time).Format(time.RFC3339), resp.Start)
		assert.Equal(t, args.Get(5).(time.Time).Format(time.RFC3339), resp.End)
	})

	t.Run("getHistoryBreakdownHandler", func(t *testing.T) {
		mockAuth, req := adminAnalyticsReq(ctx)
		t.Cleanup(func() { mockAuth.AssertExpectations(t) })
		mockClient := new(MockAnalyticsClient)
		t.Cleanup(func() { mockClient.AssertExpectations(t) })
		data := func() map[string]BreakdownValue {
			return map[string]BreakdownValue{"ec2": {TotalSavings: 50, PurchaseCount: 1, Percentage: 100}}
		}
		// No dimension param: the handler defaults to "service".
		mockClient.On("QueryBreakdown", ctx, []string(nil), map[string][]string(nil), mock.Anything, mock.Anything, "service").
			Return(data(), nil)

		h := &Handler{auth: mockAuth, analyticsClient: mockClient}
		router := NewRouter(h)

		result, err := router.getHistoryBreakdownHandler(ctx, req, map[string]string{})
		require.NoError(t, err)
		resp, ok := result.(*BreakdownResponse)
		require.True(t, ok, "result type %T", result)
		assert.Equal(t, "service", resp.Dimension)
		assert.Equal(t, data(), resp.Data)
		require.Len(t, mockClient.Calls, 1)
		args := mockClient.Calls[0].Arguments
		assert.Equal(t, args.Get(3).(time.Time).Format(time.RFC3339), resp.Start)
		assert.Equal(t, args.Get(4).(time.Time).Format(time.RFC3339), resp.End)
	})

	t.Run("triggerAnalyticsCollectionHandler", func(t *testing.T) {
		mockAuth, req := adminAnalyticsReq(ctx)
		t.Cleanup(func() { mockAuth.AssertExpectations(t) })
		mockCollector := new(MockAnalyticsCollector)
		t.Cleanup(func() { mockCollector.AssertExpectations(t) })
		mockCollector.On("Collect", ctx).Return(nil)

		h := &Handler{auth: mockAuth, analyticsCollector: mockCollector}
		router := NewRouter(h)

		result, err := router.triggerAnalyticsCollectionHandler(ctx, req, nil)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{
			"status":  "success",
			"message": "Analytics collection completed",
		}, result)
	})
}

// Test handler_plans functions.
func TestHandler_listPlans_Error(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	adminSession := &Session{UserID: "admin-id"}
	mockAuth.On("ValidateSession", ctx, "test-token").Return(adminSession, nil)
	mockAuth.grantAdmin()
	mockStore.On("ListPurchasePlans", mock.Anything, mock.Anything).Return(nil, errors.New("db error"))

	handler := &Handler{config: mockStore, auth: mockAuth}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"Authorization": "Bearer test-token"},
	}

	_, err := handler.listPlans(ctx, req, map[string]string{})
	assert.Error(t, err)
}

func TestHandler_getPlan_NotFound(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockAuth := new(MockAuthService)

	adminSession := &Session{UserID: "admin-id"}
	mockAuth.On("ValidateSession", ctx, "test-token").Return(adminSession, nil)
	mockAuth.grantAdmin()
	mockStore.On("GetPurchasePlan", mock.Anything, "11111111-1111-1111-1111-111111111111").Return(nil, errors.New("not found"))
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	handler := &Handler{config: mockStore, auth: mockAuth}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"Authorization": "Bearer test-token"},
	}

	_, err := handler.getPlan(ctx, req, "11111111-1111-1111-1111-111111111111")
	assert.Error(t, err)
}

// Tests for handler_users

func TestHandler_deleteUser_CannotDeleteSelf(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	// Use a valid UUID format
	adminID := "11111111-1111-1111-1111-111111111111"
	adminSession := &Session{UserID: adminID}
	mockAuth.On("ValidateSession", ctx, "admin-token").Return(adminSession, nil)
	mockAuth.grantAdmin()

	handler := &Handler{auth: mockAuth}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"Authorization": "Bearer admin-token"},
	}

	_, err := handler.deleteUser(ctx, req, adminID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cannot delete your own account")
}

func TestHandler_deleteUser_Error(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	adminSession := &Session{UserID: "admin-id"}
	mockAuth.On("ValidateSession", ctx, "admin-token").Return(adminSession, nil)
	mockAuth.grantAdmin()
	mockAuth.On("DeleteUser", ctx, "22222222-2222-4222-8222-222222222222").Return(errors.New("delete failed"))

	handler := &Handler{auth: mockAuth}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"Authorization": "Bearer admin-token"},
	}

	_, err := handler.deleteUser(ctx, req, "22222222-2222-4222-8222-222222222222")
	require.Error(t, err)
	assert.EqualError(t, err, "delete failed", "the store failure must surface, not an earlier validation error")
}

// Tests for handler_groups

func TestHandler_deleteGroup_DeleteFailed(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	adminSession := &Session{UserID: "admin-id"}
	mockAuth.On("ValidateSession", ctx, "admin-token").Return(adminSession, nil)
	mockAuth.grantAdmin()
	mockAuth.On("DeleteGroup", ctx, "33333333-3333-4333-8333-333333333333").Return(errors.New("delete failed"))

	handler := &Handler{auth: mockAuth}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"Authorization": "Bearer admin-token"},
	}

	_, err := handler.deleteGroup(ctx, req, "33333333-3333-4333-8333-333333333333")
	require.Error(t, err)
	assert.EqualError(t, err, "delete failed", "the store failure must surface, not an earlier validation error")
}

// Tests for forgotPassword

func TestHandler_forgotPassword_SuccessOnError(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	// Even when RequestPasswordReset returns an error, forgotPassword should return success
	// (to prevent email enumeration)
	mockAuth.On("RequestPasswordReset", ctx, "test@example.com").Return(errors.New("user not found"))

	handler := &Handler{auth: mockAuth}

	req := &events.LambdaFunctionURLRequest{Body: `{"email": "test@example.com"}`}
	result, err := handler.forgotPassword(ctx, req)
	require.NoError(t, err)

	response, ok := result.(map[string]string)
	require.True(t, ok)
	assert.Contains(t, response["status"], "if the email exists")
}

// Test for validation decodeBase64Password

func TestDecodeBase64Password_InvalidBase64(t *testing.T) {
	// Test with invalid base64
	_, err := decodeBase64Password("not-valid-base64!!!")
	assert.Error(t, err)
}

func TestDecodeBase64Password_ValidBase64(t *testing.T) {
	// "test123" encoded in base64
	result, err := decodeBase64Password("dGVzdDEyMw==")
	assert.NoError(t, err)
	assert.Equal(t, "test123", result)
}

// Test NewHandler with API key loaded.
func TestNewHandler_WithDependencies(t *testing.T) {
	mockStore := new(MockConfigStore)
	mockScheduler := new(MockScheduler)
	mockPurchase := new(MockPurchaseManager)
	mockAuth := new(MockAuthService)

	cfg := HandlerConfig{
		ConfigStore:       mockStore,
		Scheduler:         mockScheduler,
		PurchaseManager:   mockPurchase,
		AuthService:       mockAuth,
		CORSAllowedOrigin: "https://example.com",
	}

	handler := NewHandler(cfg)

	assert.NotNil(t, handler)
	assert.Equal(t, "https://example.com", handler.corsAllowedOrigin)
}

// Test handler_groups additional coverage

func TestHandler_createGroup_NoToken(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)

	handler := &Handler{auth: mockAuth}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{},
	}

	_, err := handler.createGroup(ctx, req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no authorization token")
}

func TestHandler_createGroup_InvalidBody(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	adminSession := &Session{UserID: "admin-id"}
	mockAuth.On("ValidateSession", ctx, "admin-token").Return(adminSession, nil)
	mockAuth.grantAdmin()

	handler := &Handler{auth: mockAuth}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"Authorization": "Bearer admin-token"},
		Body:    `{invalid json}`,
	}

	_, err := handler.createGroup(ctx, req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid request")
}

func TestHandler_getGroup_NoToken(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)

	handler := &Handler{auth: mockAuth}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{},
	}

	_, err := handler.getGroup(ctx, req, "11111111-1111-1111-1111-111111111111")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no authorization token")
}

// Test handler_plans additional coverage

func TestHandler_deletePlan_Success(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	adminSession := &Session{UserID: "admin-id"}
	mockAuth.On("ValidateSession", ctx, "admin-token").Return(adminSession, nil)
	mockAuth.grantAdmin()
	mockStore.On("DeletePurchasePlan", mock.Anything, "11111111-1111-1111-1111-111111111111").Return(nil)

	handler := &Handler{config: mockStore, auth: mockAuth}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"Authorization": "Bearer admin-token"},
	}

	result, err := handler.deletePlan(ctx, req, "11111111-1111-1111-1111-111111111111")
	require.NoError(t, err)

	resultMap, ok := result.(map[string]string)
	require.True(t, ok)
	assert.Equal(t, "deleted", resultMap["status"])
}

func TestHandler_deletePlan_NoToken(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)

	handler := &Handler{auth: mockAuth}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{},
	}

	_, err := handler.deletePlan(ctx, req, "11111111-1111-1111-1111-111111111111")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no authorization token")
}

func TestHandler_deletePlan_Error(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	adminSession := &Session{UserID: "admin-id"}
	mockAuth.On("ValidateSession", ctx, "admin-token").Return(adminSession, nil)
	mockAuth.grantAdmin()
	mockStore.On("DeletePurchasePlan", mock.Anything, "11111111-1111-1111-1111-111111111111").Return(errors.New("not found"))

	handler := &Handler{config: mockStore, auth: mockAuth}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"Authorization": "Bearer admin-token"},
	}

	_, err := handler.deletePlan(ctx, req, "11111111-1111-1111-1111-111111111111")
	require.Error(t, err)
}
