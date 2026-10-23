//go:build integration

package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	rotationPassword    = "SyntheticPassword123!"
	rotationNewPassword = "RotatedPassword456#"
)

// failingKeyListStore makes the best-effort revocation scan after a password
// change fail, as a transient database error would.
type failingKeyListStore struct{ *PostgresStore }

func (failingKeyListStore) ListAPIKeysByUser(context.Context, string) ([]*UserAPIKey, error) {
	return nil, errors.New("synthetic list failure")
}

// userReadHook runs onRead once, right after the first GetUserByID returns.
type userReadHook struct {
	*PostgresStore
	onRead func()
}

func (s *userReadHook) GetUserByID(ctx context.Context, id string) (*User, error) {
	user, err := s.PostgresStore.GetUserByID(ctx, id)
	if hook := s.onRead; hook != nil {
		s.onRead = nil
		hook()
	}
	return user, err
}

func newRotationService(store StoreInterface) *Service {
	svc := NewService(ServiceConfig{Store: store})
	svc.bcryptCostOverride = 4
	return svc
}

func createRotationUser(t *testing.T, svc *Service, store *PostgresStore, email string) *User {
	t.Helper()
	hash, err := svc.hashPassword(rotationPassword)
	require.NoError(t, err)
	user := &User{Email: email, PasswordHash: hash, Active: true, GroupIDs: []string{DefaultPurchaserGroupID}}
	require.NoError(t, store.CreateUser(t.Context(), user))
	return user
}

func TestIntegration_APIKeyRejectedAfterPasswordRotation(t *testing.T) {
	store := NewPostgresStore(setupAuthTestDB(t))
	svc := newRotationService(store)
	ctx := t.Context()
	expiry := time.Now().Add(time.Hour)
	perms := []Permission{{Action: ActionView, Resource: ResourceRecommendations}}
	change := ChangePasswordRequest{CurrentPassword: rotationPassword, NewPassword: rotationNewPassword}

	t.Run("revocation-scan-fails", func(t *testing.T) {
		user := createRotationUser(t, svc, store, "rotation-scan-fails@example.com")
		apiKey, _, err := svc.CreateAPIKey(ctx, user.ID, "ci", rotationPassword, perms, &expiry)
		require.NoError(t, err)
		require.NoError(t, newRotationService(failingKeyListStore{store}).ChangePassword(ctx, user.ID, change))

		_, _, err = svc.ValidateUserAPIKey(ctx, apiKey)
		require.ErrorIs(t, err, ErrAPIKeyPasswordRotated)

		// The row is still is_active=true, but the listing must not show a
		// key the authentication path rejects as active (issue #534).
		listed, err := svc.ListUserAPIKeysAPI(ctx, user.ID)
		require.NoError(t, err)
		keys := listed.(*APIListAPIKeysResponse).APIKeys
		require.Len(t, keys, 1)
		assert.False(t, keys[0].IsActive, "a key rejected for predating the password change must not list as active")
	})

	t.Run("key-minted-across-rotation", func(t *testing.T) {
		user := createRotationUser(t, svc, store, "rotation-mint-race@example.com")
		racing := &userReadHook{PostgresStore: store, onRead: func() {
			require.NoError(t, svc.ChangePassword(ctx, user.ID, change))
		}}
		apiKey, _, err := newRotationService(racing).CreateAPIKey(ctx, user.ID, "ci", rotationPassword, perms, &expiry)
		require.NoError(t, err, "the mint verified the password it read before the rotation")

		_, _, err = svc.ValidateUserAPIKey(ctx, apiKey)
		require.ErrorIs(t, err, ErrAPIKeyPasswordRotated)
	})

	// The listing reads the user row before the keys. A rotation plus a fresh
	// mint landing between the two reads gives a key newer than the listing's
	// user snapshot; it is valid and must not be shown as stale (issue #534).
	t.Run("key-minted-while-listing", func(t *testing.T) {
		user := createRotationUser(t, svc, store, "rotation-list-race@example.com")
		_, _, err := svc.CreateAPIKey(ctx, user.ID, "old", rotationPassword, perms, &expiry)
		require.NoError(t, err)
		racing := &userReadHook{PostgresStore: store, onRead: func() {
			require.NoError(t, svc.ChangePassword(ctx, user.ID, change))
			_, _, mintErr := svc.CreateAPIKey(ctx, user.ID, "new", rotationNewPassword, perms, &expiry)
			require.NoError(t, mintErr)
		}}

		listed, err := newRotationService(racing).ListUserAPIKeysAPI(ctx, user.ID)
		require.NoError(t, err)
		active := map[string]bool{}
		for _, k := range listed.(*APIListAPIKeysResponse).APIKeys {
			active[k.Name] = k.IsActive
		}
		assert.Equal(t, map[string]bool{"old": false, "new": true}, active)
	})

	t.Run("key-minted-after-rotation", func(t *testing.T) {
		user := createRotationUser(t, svc, store, "rotation-fresh-key@example.com")
		before, _, err := svc.CreateAPIKey(ctx, user.ID, "old", rotationPassword, perms, &expiry)
		require.NoError(t, err)
		require.NoError(t, svc.UpdateUserProfile(ctx, user.ID, "rotation-fresh-key-renamed@example.com", rotationPassword, ""))
		_, _, err = svc.ValidateUserAPIKey(ctx, before)
		require.NoError(t, err, "a write that keeps the password must not invalidate keys")

		require.NoError(t, svc.ChangePassword(ctx, user.ID, change))
		after, _, err := svc.CreateAPIKey(ctx, user.ID, "new", rotationNewPassword, perms, &expiry)
		require.NoError(t, err)
		key, owner, err := svc.ValidateUserAPIKey(ctx, after)
		require.NoError(t, err)
		assert.Equal(t, user.ID, owner.ID)
		assert.Equal(t, "new", key.Name)
	})
}
