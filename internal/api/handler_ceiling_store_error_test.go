package api

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/aws/aws-lambda-go/events"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A store failure while resolving the acting user's permissions must reach the
// caller as the plain 403, even when the caller may view groups.

const ceilingStoreFailure = "failed to connect to `user=app_user database=app`: 10.1.2.3:5432 (10.1.2.3): dial error"

// failingActorStore fails the acting user's lookup; the target-email lookup
// reports "no such user" so a create reaches the grant check.
type failingActorStore struct {
	auth.StoreInterface
}

func (failingActorStore) GetUserByEmail(context.Context, string) (*auth.User, error) {
	return nil, pgx.ErrNoRows
}

func (failingActorStore) GetUserByID(context.Context, string) (*auth.User, error) {
	return nil, errors.New(ceilingStoreFailure)
}

// realWriteAuth routes the user and group create entry points to a real
// auth.Service and keeps everything else on the mock.
type realWriteAuth struct {
	*MockAuthService
	svc *auth.Service
}

func (a realWriteAuth) CreateUserAPI(ctx context.Context, actor string, req auth.APICreateUserRequest) (*auth.APICreateUserResponse, error) {
	return a.svc.CreateUserAPI(ctx, actor, req)
}

func (a realWriteAuth) CreateGroupAPI(ctx context.Context, actor string, req auth.APICreateGroupRequest) (*auth.APIGroup, error) {
	return a.svc.CreateGroupAPI(ctx, actor, req)
}

func TestHandler_writes_CeilingStoreErrorOmitsDetailForGroupViewer(t *testing.T) {
	ctx := context.Background()
	session := &Session{UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Email: "ops@example.com"}
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", ctx, "tok").Return(session, nil)
	mockAuth.grantAdmin()
	svc := auth.NewService(auth.ServiceConfig{Store: failingActorStore{}})
	h := &Handler{auth: realWriteAuth{MockAuthService: mockAuth, svc: svc}}
	headers := map[string]string{"Authorization": "Bearer tok"}
	password := base64.StdEncoding.EncodeToString([]byte("Zk9#vQ2mXr!41wLp"))

	cases := map[string]func() error{
		"create user": func() error {
			_, err := h.createUser(ctx, &events.LambdaFunctionURLRequest{Headers: headers,
				Body: `{"email": "new@example.com", "password": "` + password + `", "groups": ["00000000-0000-5000-8000-000000000005"]}`})
			return err
		},
		"create group": func() error {
			_, err := h.createGroup(ctx, &events.LambdaFunctionURLRequest{Headers: headers,
				Body: `{"name": "Readers", "permissions": [{"action": "view", "resource": "recommendations"}], "allowed_accounts": ["*"]}`})
			return err
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			err := call()

			ce, ok := IsClientError(err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, 403, ce.code)
			for _, needle := range []string{"10.1.2.3", "app_user", "dial"} {
				assert.NotContains(t, err.Error(), needle)
			}
		})
	}
}
