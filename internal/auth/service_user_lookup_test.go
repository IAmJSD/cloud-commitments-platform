package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lookupCaller is one service entry point that resolves a user by ID before
// doing anything else. setup stubs whatever the caller reads before that
// lookup; call runs it against lookupUserID.
type lookupCaller struct {
	name       string
	activeOnly bool
	setup      func(ctx context.Context, m *MockStore)
	call       func(ctx context.Context, s *Service) error
}

const (
	lookupUserID = "lookup-user"
	lookupAPIKey = "lookup-api-key"
)

// lookupCallers lists every caller routed through loadUser / loadActiveUser
// (issue #313). Before that, each one carried its own copy of the lookup and
// they disagreed on the not-found message ("user account not found" on the
// API-key path) and on whether a store error was wrapped.
func lookupCallers() []lookupCaller {
	noSetup := func(context.Context, *MockStore) {}
	ownKey := &UserAPIKey{ID: "key-1", UserID: lookupUserID, IsActive: true}
	return []lookupCaller{
		{name: "UpdateUser", setup: noSetup, call: func(ctx context.Context, s *Service) error {
			_, err := s.UpdateUser(ctx, "", lookupUserID, UpdateUserRequest{})
			return err
		}},
		{name: "DeleteUser", setup: noSetup, call: func(ctx context.Context, s *Service) error {
			return s.DeleteUser(ctx, lookupUserID)
		}},
		{name: "UpdateUserProfile", setup: noSetup, call: func(ctx context.Context, s *Service) error {
			return s.UpdateUserProfile(ctx, lookupUserID, "", "current", "")
		}},
		{name: "ChangePassword", setup: noSetup, call: func(ctx context.Context, s *Service) error {
			return s.ChangePassword(ctx, lookupUserID, ChangePasswordRequest{CurrentPassword: "current"})
		}},
		{name: "GetUserPermissions", setup: noSetup, call: func(ctx context.Context, s *Service) error {
			_, err := s.GetUserPermissions(ctx, lookupUserID)
			return err
		}},
		{name: "BuildAuthContext", setup: noSetup, call: func(ctx context.Context, s *Service) error {
			_, err := s.BuildAuthContext(ctx, lookupUserID)
			return err
		}},
		{name: "ListUserAPIKeys", setup: noSetup, call: func(ctx context.Context, s *Service) error {
			_, err := s.ListUserAPIKeys(ctx, lookupUserID)
			return err
		}},
		{name: "RevokeAPIKey", setup: func(ctx context.Context, m *MockStore) {
			m.On("GetAPIKeyByID", ctx, "key-1").Return(ownKey, nil)
		}, call: func(ctx context.Context, s *Service) error {
			return s.RevokeAPIKey(ctx, lookupUserID, "key-1")
		}},
		{name: "CreateAPIKey", activeOnly: true, setup: noSetup, call: func(ctx context.Context, s *Service) error {
			_, _, err := s.CreateAPIKey(ctx, lookupUserID, "k", "current", nil, nil)
			return err
		}},
		{name: "ValidateUserAPIKey", activeOnly: true, setup: func(ctx context.Context, m *MockStore) {
			// A live, unexpired key, so validation reaches the owner lookup.
			sum := sha256.Sum256([]byte(lookupAPIKey))
			hash := base64.RawURLEncoding.EncodeToString(sum[:])
			expires := time.Now().Add(time.Hour)
			m.On("GetAPIKeyByHash", ctx, hash).Return(&UserAPIKey{
				ID: "key-1", UserID: lookupUserID, KeyHash: hash, IsActive: true, ExpiresAt: &expires,
			}, nil)
		}, call: func(ctx context.Context, s *Service) error {
			_, _, err := s.ValidateUserAPIKey(ctx, lookupAPIKey)
			return err
		}},
	}
}

func newLookupService(ctx context.Context, t *testing.T, c lookupCaller, user *User, err error) *Service {
	t.Helper()
	m := new(MockStore)
	c.setup(ctx, m)
	m.On("GetUserByID", ctx, lookupUserID).Return(user, err)
	return createTestService(m, new(MockEmailSender))
}

// TestUserLookup_UniformNotFoundAndStoreError pins one lookup contract on every
// caller: a missing row (pgx.ErrNoRows or a nil user) reads "user not found",
// and any other store error is wrapped as "failed to get user: %w".
func TestUserLookup_UniformNotFoundAndStoreError(t *testing.T) {
	ctx := context.Background()
	storeErr := errors.New("connection reset")

	for _, c := range lookupCallers() {
		t.Run(c.name+"/ErrNoRows", func(t *testing.T) {
			err := c.call(ctx, newLookupService(ctx, t, c, nil, pgx.ErrNoRows))
			require.EqualError(t, err, "user not found")
		})
		t.Run(c.name+"/nil user", func(t *testing.T) {
			err := c.call(ctx, newLookupService(ctx, t, c, nil, nil))
			require.EqualError(t, err, "user not found")
		})
		t.Run(c.name+"/store error", func(t *testing.T) {
			err := c.call(ctx, newLookupService(ctx, t, c, nil, storeErr))
			require.ErrorIs(t, err, storeErr)
			assert.EqualError(t, err, "failed to get user: connection reset")
		})
	}
}

// TestUserLookup_ActiveOnlyCallersRefuseInactiveUser: the API-key mint and
// authenticate paths go through loadActiveUser and refuse a deactivated
// owner with the same message.
func TestUserLookup_ActiveOnlyCallersRefuseInactiveUser(t *testing.T) {
	ctx := context.Background()
	for _, c := range lookupCallers() {
		if !c.activeOnly {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			inactive := &User{ID: lookupUserID, Email: "inactive@example.com", Active: false}
			err := c.call(ctx, newLookupService(ctx, t, c, inactive, nil))
			require.EqualError(t, err, "user account is not active")
		})
	}
}
