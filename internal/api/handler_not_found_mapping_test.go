package api

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/aws/aws-lambda-go/events"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #312 (audit A02-012): a missing user or group, and a missing or
// someone else's API key, answered 500 "Internal server error" (and logged at
// error level) where openapi.yaml documents 404. The auth service now returns
// exported sentinels; these tests pin the handler mapping. Each error below is
// the exact shape the real service returns.

const notFoundTestID = "11111111-1111-1111-1111-111111111111"

func notFoundAdminReq(ctx context.Context, path string) (*MockAuthService, *events.LambdaFunctionURLRequest) {
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", ctx, "admin-token").Return(&Session{
		UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		Email:  "admin@example.com",
	}, nil)
	mockAuth.grantAdmin()
	return mockAuth, &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"Authorization": "Bearer admin-token"},
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Path: path},
		},
	}
}

func requireNotFound(t *testing.T, err error, msg string) {
	t.Helper()
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "must be a client error, not a 500: %v", err)
	assert.Equal(t, 404, ce.code)
	assert.Equal(t, msg, ce.message)
}

func requireServerError(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	_, ok := IsClientError(err)
	assert.False(t, ok, "a store failure must stay a 500, got client error %v", err)
}

func TestHandler_getUser_MissingUserIs404(t *testing.T) {
	ctx := context.Background()
	mockAuth, req := notFoundAdminReq(ctx, "/api/users/"+notFoundTestID)
	mockAuth.On("GetUser", ctx, notFoundTestID).
		Return(nil, fmt.Errorf("%w: %w", auth.ErrUserNotFound, pgx.ErrNoRows))
	h := &Handler{auth: mockAuth}

	_, err := h.getUser(ctx, req, notFoundTestID)

	requireNotFound(t, err, "user not found")
}

func TestHandler_getUser_StoreFailureStays500(t *testing.T) {
	ctx := context.Background()
	mockAuth, req := notFoundAdminReq(ctx, "/api/users/"+notFoundTestID)
	mockAuth.On("GetUser", ctx, notFoundTestID).Return(nil, errors.New("connection refused"))
	h := &Handler{auth: mockAuth}

	_, err := h.getUser(ctx, req, notFoundTestID)

	requireServerError(t, err)
}

func TestHandler_getGroup_MissingGroupIs404(t *testing.T) {
	ctx := context.Background()
	for name, storeErr := range map[string]error{
		"no rows":   fmt.Errorf("%w: %w", auth.ErrGroupNotFound, pgx.ErrNoRows),
		"nil group": auth.ErrGroupNotFound,
	} {
		t.Run(name, func(t *testing.T) {
			mockAuth, req := notFoundAdminReq(ctx, "/api/groups/"+notFoundTestID)
			mockAuth.On("GetGroupAPI", ctx, notFoundTestID).Return(nil, storeErr)
			h := &Handler{auth: mockAuth}

			_, err := h.getGroup(ctx, req, notFoundTestID)

			requireNotFound(t, err, "group not found")
		})
	}
}

func TestHandler_getGroup_StoreFailureStays500(t *testing.T) {
	ctx := context.Background()
	mockAuth, req := notFoundAdminReq(ctx, "/api/groups/"+notFoundTestID)
	mockAuth.On("GetGroupAPI", ctx, notFoundTestID).Return(nil, errors.New("connection refused"))
	h := &Handler{auth: mockAuth}

	_, err := h.getGroup(ctx, req, notFoundTestID)

	requireServerError(t, err)
}

// The not-owned case gets the same 404 and message as a missing key, so a
// caller cannot tell another user's key ID from one that does not exist.
func TestHandler_APIKeyDeleteRevoke_MissingOrNotOwnedIs404(t *testing.T) {
	ctx := context.Background()
	serviceErrs := map[string]func(action string) error{
		"missing": func(string) error { return auth.ErrAPIKeyNotFound },
		"not owned": func(action string) error {
			return fmt.Errorf("%w: cannot %s another user's API key", auth.ErrAPIKeyNotOwned, action)
		},
	}
	for name, serviceErr := range serviceErrs {
		t.Run("delete "+name, func(t *testing.T) {
			mockAuth, req := notFoundAdminReq(ctx, "/api/api-keys/"+notFoundTestID)
			mockAuth.On("DeleteAPIKeyAPI", ctx, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", notFoundTestID).Return(serviceErr("delete"))
			h := &Handler{auth: mockAuth}

			_, err := h.deleteAPIKey(ctx, req)

			requireNotFound(t, err, "API key not found")
		})
		t.Run("revoke "+name, func(t *testing.T) {
			mockAuth, req := notFoundAdminReq(ctx, "/api/api-keys/"+notFoundTestID+"/revoke")
			mockAuth.On("RevokeAPIKeyAPI", ctx, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", notFoundTestID).Return(serviceErr("revoke"))
			h := &Handler{auth: mockAuth}

			_, err := h.revokeAPIKey(ctx, req)

			requireNotFound(t, err, "API key not found")
		})
	}
}

func TestHandler_APIKeyDeleteRevoke_StoreFailureStays500(t *testing.T) {
	ctx := context.Background()
	storeErr := errors.New("failed to get API key: connection refused")

	mockAuth, req := notFoundAdminReq(ctx, "/api/api-keys/"+notFoundTestID)
	mockAuth.On("DeleteAPIKeyAPI", ctx, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", notFoundTestID).Return(storeErr)
	_, err := (&Handler{auth: mockAuth}).deleteAPIKey(ctx, req)
	requireServerError(t, err)

	mockAuth, req = notFoundAdminReq(ctx, "/api/api-keys/"+notFoundTestID+"/revoke")
	mockAuth.On("RevokeAPIKeyAPI", ctx, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", notFoundTestID).Return(storeErr)
	_, err = (&Handler{auth: mockAuth}).revokeAPIKey(ctx, req)
	requireServerError(t, err)
}
