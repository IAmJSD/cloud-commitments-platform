//go:build integration

package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// failingInvalidationStore makes both best-effort cleanups after a password
// change fail, as a transient database error would (issue #398).
type failingInvalidationStore struct{ failingKeyListStore }

func (failingInvalidationStore) DeleteUserSessions(context.Context, string) error {
	return errors.New("synthetic session delete failure")
}

// sessionReadHook runs onRead once, right after the first GetSession returns.
type sessionReadHook struct {
	*PostgresStore
	onRead func()
}

func (s *sessionReadHook) GetSession(ctx context.Context, token string) (*Session, error) {
	session, err := s.PostgresStore.GetSession(ctx, token)
	if hook := s.onRead; hook != nil {
		s.onRead = nil
		hook()
	}
	return session, err
}

func loginRotationUser(t *testing.T, svc *Service, email, password string) string {
	t.Helper()
	resp, err := svc.Login(t.Context(), LoginRequest{Email: email, Password: password})
	require.NoError(t, err)
	return resp.Token
}

// rotatePassword changes the user's password through each service entry point
// that rotates it, using svc for the write.
var rotatePassword = map[string]func(t *testing.T, svc *Service, store *PostgresStore, user *User){
	"change-password": func(t *testing.T, svc *Service, _ *PostgresStore, user *User) {
		require.NoError(t, svc.ChangePassword(t.Context(), user.ID,
			ChangePasswordRequest{CurrentPassword: rotationPassword, NewPassword: rotationNewPassword}))
	},
	"update-profile": func(t *testing.T, svc *Service, _ *PostgresStore, user *User) {
		require.NoError(t, svc.UpdateUserProfile(t.Context(), user.ID, user.Email, rotationPassword, rotationNewPassword))
	},
	"reset-confirm": func(t *testing.T, svc *Service, store *PostgresStore, user *User) {
		token := "session-rotation-reset-" + user.ID
		_, err := store.db.Exec(t.Context(), `UPDATE users SET password_reset_token = $2,
			password_reset_expiry = NOW() + interval '1 hour' WHERE id = $1`, user.ID, hashSessionToken(token))
		require.NoError(t, err)
		require.NoError(t, svc.ConfirmPasswordReset(t.Context(), PasswordResetConfirm{Token: token, NewPassword: rotationNewPassword}))
	},
}

func TestIntegration_SessionRejectedAfterPasswordRotation(t *testing.T) {
	store := NewPostgresStore(setupAuthTestDB(t))
	svc := newRotationService(store)
	ctx := t.Context()
	change := ChangePasswordRequest{CurrentPassword: rotationPassword, NewPassword: rotationNewPassword}
	expiry := time.Now().Add(time.Hour)
	perms := []Permission{{Action: ActionView, Resource: ResourceRecommendations}}

	for name, rotate := range rotatePassword {
		t.Run("invalidation-fails/"+name, func(t *testing.T) {
			user := createRotationUser(t, svc, store, "session-invalidation-"+name+"@example.com")
			session := loginRotationUser(t, svc, user.Email, rotationPassword)
			apiKey, _, err := svc.CreateAPIKey(ctx, user.ID, "ci", rotationPassword, perms, &expiry)
			require.NoError(t, err)
			rotate(t, newRotationService(failingInvalidationStore{failingKeyListStore{store}}), store, user)

			_, err = svc.ValidateSession(ctx, session)
			require.ErrorIs(t, err, ErrSessionPasswordRotated)
			_, _, err = svc.ValidateUserAPIKey(ctx, apiKey)
			require.ErrorIs(t, err, ErrAPIKeyPasswordRotated, "already enforced by migration 000103 (issue #402)")

			fresh := loginRotationUser(t, svc, user.Email, rotationNewPassword)
			_, err = svc.ValidateSession(ctx, fresh)
			require.NoError(t, err, "a session issued after the change must stay valid")
		})
	}

	t.Run("login-across-rotation", func(t *testing.T) {
		user := createRotationUser(t, svc, store, "session-login-race@example.com")
		racing := &credentialReadBarrier{StoreInterface: store, afterRead: func(ctx context.Context, _ *User) {
			require.NoError(t, svc.ChangePassword(ctx, user.ID, change))
		}}
		session := loginRotationUser(t, newRotationService(racing), user.Email, rotationPassword)

		_, err := svc.ValidateSession(ctx, session)
		require.ErrorIs(t, err, ErrSessionPasswordRotated,
			"the login verified the password it read before the rotation, after the session sweep ran")
	})

	t.Run("session-use-across-rotation", func(t *testing.T) {
		user := createRotationUser(t, svc, store, "session-use-race@example.com")
		session := loginRotationUser(t, svc, user.Email, rotationPassword)
		racing := &sessionReadHook{PostgresStore: store, onRead: func() {
			require.NoError(t, svc.ChangePassword(ctx, user.ID, change))
		}}

		_, err := newRotationService(racing).ValidateSession(ctx, session)
		require.ErrorIs(t, err, ErrSessionPasswordRotated)
	})

	t.Run("concurrent-logins", func(t *testing.T) {
		user := createRotationUser(t, svc, store, "session-concurrent-logins@example.com")
		var (
			mu     sync.Mutex
			tokens []string
			wg     sync.WaitGroup
		)
		started := make(chan struct{})
		var once sync.Once
		// Each client logs in with the old password until the rotation makes
		// it fail, so some logins straddle the rotation's commit and sweep.
		for range 16 {
			wg.Go(func() {
				for range 1000 {
					resp, err := svc.Login(ctx, LoginRequest{Email: user.Email, Password: rotationPassword})
					if err != nil {
						return
					}
					mu.Lock()
					tokens = append(tokens, resp.Token)
					mu.Unlock()
					once.Do(func() { close(started) })
				}
			})
		}
		<-started
		require.NoError(t, svc.ChangePassword(ctx, user.ID, change))
		wg.Wait()

		// A session minted before the sweep is deleted; one minted after it
		// survives the sweep and must be rejected by its password version.
		for _, token := range tokens {
			_, err := svc.ValidateSession(ctx, token)
			require.Error(t, err, "every session from the old password must be rejected")
		}
	})
}
