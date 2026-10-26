package api

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestHandler_listUsers_Success(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)

	adminSession := &Session{
		UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		Email:  "admin@example.com",
	}

	users := []interface{}{
		map[string]interface{}{"id": "11111111-1111-1111-1111-111111111111", "email": "user1@example.com"},
		map[string]interface{}{"id": "22222222-2222-2222-2222-222222222222", "email": "user2@example.com"},
	}

	mockAuth.On("ValidateSession", ctx, "admin-token").Return(adminSession, nil)
	mockAuth.grantAdmin()
	mockAuth.On("ListUsersAPI", ctx).Return(users, nil)

	handler := &Handler{auth: mockAuth}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{
			"Authorization": "Bearer admin-token",
		},
	}

	result, err := handler.listUsers(ctx, req)
	require.NoError(t, err)

	resp := result.(map[string]interface{})
	assert.NotNil(t, resp["users"])
}

func TestHandler_listUsers_NoPermission(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)

	userSession := &Session{
		UserID: "11111111-1111-1111-1111-111111111111",
		Email:  "user@example.com",
	}

	mockAuth.On("ValidateSession", ctx, "user-token").Return(userSession, nil)
	mockAuth.On("HasPermissionAPI", ctx, "11111111-1111-1111-1111-111111111111", "view", "users").Return(false, nil)

	handler := &Handler{auth: mockAuth}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{
			"Authorization": "Bearer user-token",
		},
	}

	result, err := handler.listUsers(ctx, req)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "permission denied")
}

func TestHandler_createUser_Success(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)

	adminSession := &Session{
		UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		Email:  "admin@example.com",
	}

	createdUser := &auth.APICreateUserResponse{APIUser: &auth.APIUser{
		ID:     "33333333-3333-3333-3333-333333333333",
		Email:  "newuser@example.com",
		Groups: []string{"00000000-0000-5000-8000-000000000005"},
	}}

	mockAuth.On("ValidateSession", ctx, "admin-token").Return(adminSession, nil)
	mockAuth.grantAdmin()
	mockAuth.On("CreateUserAPI", ctx, mock.Anything, mock.Anything).Return(createdUser, nil)

	handler := &Handler{auth: mockAuth}

	password := base64.StdEncoding.EncodeToString([]byte("password123"))
	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{
			"Authorization": "Bearer admin-token",
		},
		Body: `{"email": "newuser@example.com", "password": "` + password + `", "groups": ["00000000-0000-5000-8000-000000000005"]}`,
	}

	result, err := handler.createUser(ctx, req)
	require.NoError(t, err)
	assert.NotNil(t, result)
}

func TestHandler_getUser_Success(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)

	adminSession := &Session{
		UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		Email:  "admin@example.com",
	}

	user := &User{
		ID:     "11111111-1111-1111-1111-111111111111",
		Email:  "user@example.com",
		Groups: []string{"00000000-0000-5000-8000-000000000005"},
	}

	mockAuth.On("ValidateSession", ctx, "admin-token").Return(adminSession, nil)
	mockAuth.grantAdmin()
	mockAuth.On("GetUser", ctx, "11111111-1111-1111-1111-111111111111").Return(user, nil)

	handler := &Handler{auth: mockAuth}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{
			"Authorization": "Bearer admin-token",
		},
	}

	result, err := handler.getUser(ctx, req, "11111111-1111-1111-1111-111111111111")
	require.NoError(t, err)

	userResult := result.(*User)
	assert.Equal(t, "11111111-1111-1111-1111-111111111111", userResult.ID)
}

func TestHandler_updateUser_Success(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)

	adminSession := &Session{
		UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		Email:  "admin@example.com",
	}

	updatedUser := &auth.APIUser{
		ID:     "11111111-1111-1111-1111-111111111111",
		Email:  "user@example.com",
		Groups: []string{"00000000-0000-5000-8000-000000000001"},
	}

	mockAuth.On("ValidateSession", ctx, "admin-token").Return(adminSession, nil)
	mockAuth.grantAdmin()
	mockAuth.On("UpdateUserAPI", ctx, adminSession.UserID, "11111111-1111-1111-1111-111111111111", mock.Anything).Return(updatedUser, nil)

	handler := &Handler{auth: mockAuth}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{
			"Authorization": "Bearer admin-token",
		},
		Body: `{"groups": ["00000000-0000-5000-8000-000000000001"]}`,
	}

	result, err := handler.updateUser(ctx, req, "11111111-1111-1111-1111-111111111111")
	require.NoError(t, err)
	assert.NotNil(t, result)
}

// TestHandler_updateUser_Deactivate is the regression test for issue #89:
// before auth.APIUpdateUserRequest gained an Active field, `{"active":
// false}` in the PUT body decoded into a struct with nowhere to put it and
// was silently dropped -- the same defect class issue #892 fixed for email.
// We assert the request auth.UpdateUserAPI actually receives carries
// Active pointing at false, not just that the call happens with
// mock.Anything, so this test would fail against the pre-fix struct (the
// unmarshal would succeed but req.Active would not exist / always be nil).
func TestHandler_updateUser_Deactivate(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)

	adminSession := &Session{
		UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		Email:  "admin@example.com",
	}

	updatedUser := &auth.APIUser{
		ID:     "11111111-1111-1111-1111-111111111111",
		Email:  "user@example.com",
		Active: false,
	}

	mockAuth.On("ValidateSession", ctx, "admin-token").Return(adminSession, nil)
	mockAuth.grantAdmin()
	mockAuth.On("UpdateUserAPI", ctx, adminSession.UserID, "11111111-1111-1111-1111-111111111111",
		mock.MatchedBy(func(req auth.APIUpdateUserRequest) bool {
			return req.Active != nil && !*req.Active
		}),
	).Return(updatedUser, nil)

	handler := &Handler{auth: mockAuth}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{
			"Authorization": "Bearer admin-token",
		},
		Body: `{"active": false}`,
	}

	result, err := handler.updateUser(ctx, req, "11111111-1111-1111-1111-111111111111")
	require.NoError(t, err)
	assert.NotNil(t, result)
}

func TestHandler_deleteUser_Success(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)

	adminSession := &Session{
		UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		Email:  "admin@example.com",
	}

	mockAuth.On("ValidateSession", ctx, "admin-token").Return(adminSession, nil)
	mockAuth.grantAdmin()
	mockAuth.On("DeleteUser", ctx, "22222222-2222-2222-2222-222222222222").Return(nil)

	handler := &Handler{auth: mockAuth}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{
			"Authorization": "Bearer admin-token",
		},
	}

	result, err := handler.deleteUser(ctx, req, "22222222-2222-2222-2222-222222222222")
	require.NoError(t, err)

	resp := result.(map[string]string)
	assert.Equal(t, "user deleted", resp["status"])
}

func TestHandler_deleteUser_SelfDeletion(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)

	adminSession := &Session{
		UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		Email:  "admin@example.com",
	}

	mockAuth.On("ValidateSession", ctx, "admin-token").Return(adminSession, nil)
	mockAuth.grantAdmin()

	handler := &Handler{auth: mockAuth}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{
			"Authorization": "Bearer admin-token",
		},
	}

	result, err := handler.deleteUser(ctx, req, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "cannot delete your own account")
}

// Group management endpoint tests.
func TestHandler_createUser_InvalidJSON(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)

	adminSession := &Session{UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}
	mockAuth.On("ValidateSession", ctx, "admin-token").Return(adminSession, nil)
	mockAuth.grantAdmin()

	handler := &Handler{auth: mockAuth}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"Authorization": "Bearer admin-token"},
		Body:    `{invalid json}`,
	}

	result, err := handler.createUser(ctx, req)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "invalid request body")
}

func TestHandler_updateUser_InvalidJSON(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)

	adminSession := &Session{UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}
	mockAuth.On("ValidateSession", ctx, "admin-token").Return(adminSession, nil)
	mockAuth.grantAdmin()

	handler := &Handler{auth: mockAuth}

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"Authorization": "Bearer admin-token"},
		Body:    `{invalid json}`,
	}

	result, err := handler.updateUser(ctx, req, "11111111-1111-1111-1111-111111111111")
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "invalid request body")
}

// Issue #78: a contradictory view-only + write-capable membership refused by
// the auth service must reach a non-UI API consumer as a 400 carrying the
// explanation, on both the create and the update path, not as a generic 500.
func TestHandler_users_ContradictoryGroupsMapTo400(t *testing.T) {
	ctx := context.Background()
	adminSession := &Session{UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Email: "admin@example.com"}
	refusal := fmt.Errorf(`%w: "Viewers" is view-only but "Plan Authors" grants write permissions; assign a view-only group or write-capable groups, not both`,
		auth.ErrContradictoryGroups)
	groups := `["00000000-0000-5000-8000-000000000004", "00000000-0000-5000-8000-000000000003"]`
	headers := map[string]string{"Authorization": "Bearer admin-token"}

	calls := map[string]func(h *Handler, m *MockAuthService) (any, error){
		"create": func(h *Handler, m *MockAuthService) (any, error) {
			m.On("CreateUserAPI", ctx, adminSession.UserID, mock.Anything).Return(nil, refusal).Once()
			password := base64.StdEncoding.EncodeToString([]byte("password123"))
			return h.createUser(ctx, &events.LambdaFunctionURLRequest{Headers: headers,
				Body: `{"email": "new@example.com", "password": "` + password + `", "groups": ` + groups + `}`})
		},
		"update": func(h *Handler, m *MockAuthService) (any, error) {
			const userID = "11111111-1111-1111-1111-111111111111"
			m.On("UpdateUserAPI", ctx, adminSession.UserID, userID, mock.Anything).Return(nil, refusal).Once()
			return h.updateUser(ctx, &events.LambdaFunctionURLRequest{Headers: headers, Body: `{"groups": ` + groups + `}`}, userID)
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			mockAuth := new(MockAuthService)
			t.Cleanup(func() { mockAuth.AssertExpectations(t) })
			mockAuth.On("ValidateSession", ctx, "admin-token").Return(adminSession, nil)
			mockAuth.grantAdmin()

			result, err := call(&Handler{auth: mockAuth}, mockAuth)

			require.Error(t, err)
			assert.Nil(t, result)
			ce, ok := IsClientError(err)
			require.True(t, ok, "a contradictory combination must map to a client error, not a 500")
			assert.Equal(t, 400, ce.code)
			assert.Contains(t, err.Error(), `"Viewers" is view-only`)
		})
	}
}
