//go:build integration

package auth

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	credentialRacePassword = "OriginalPassword123!"
	credentialRaceNew      = "ReplacedPassword456!"
)

// credentialReadBarrier runs afterRead once, between the service's user read
// and its write, to replay the stale-snapshot schedules from issue #474.
type credentialReadBarrier struct {
	StoreInterface
	afterRead func(context.Context, *User)
}

func (s *credentialReadBarrier) pause(ctx context.Context, u *User) {
	if s.afterRead != nil {
		f := s.afterRead
		s.afterRead = nil
		f(ctx, u)
	}
}

func (s *credentialReadBarrier) GetUserByID(ctx context.Context, id string) (*User, error) {
	u, err := s.StoreInterface.GetUserByID(ctx, id)
	if err == nil {
		s.pause(ctx, u)
	}
	return u, err
}

func (s *credentialReadBarrier) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	u, err := s.StoreInterface.GetUserByEmail(ctx, email)
	if err == nil {
		s.pause(ctx, u)
	}
	return u, err
}

func (s *credentialReadBarrier) GetUserByResetToken(ctx context.Context, token string) (*User, error) {
	u, err := s.StoreInterface.GetUserByResetToken(ctx, token)
	if err == nil {
		s.pause(ctx, u)
	}
	return u, err
}

type recordingMailSink struct {
	resets []string
	err    error
}

func (m *recordingMailSink) SendPasswordResetEmail(_ context.Context, email, _ string) error {
	m.resets = append(m.resets, email)
	return m.err
}
func (m *recordingMailSink) SendWelcomeEmail(context.Context, string, string, string) error {
	return nil
}
func (m *recordingMailSink) SendUserInviteEmail(context.Context, string, string) error { return nil }

type credentialRaceFixture struct {
	t       *testing.T
	store   *PostgresStore
	barrier *credentialReadBarrier
	mail    *recordingMailSink
	svc     *Service
	user    *User
	session string
}

func newCredentialRaceFixture(t *testing.T, store *PostgresStore, email string) *credentialRaceFixture {
	f := &credentialRaceFixture{t: t, store: store, barrier: &credentialReadBarrier{StoreInterface: store}, mail: &recordingMailSink{}}
	f.svc = f.newService(f.barrier, f.mail)
	hash, err := f.svc.hashPassword(credentialRacePassword)
	require.NoError(t, err)
	f.user = &User{Email: email, PasswordHash: hash, Active: true, GroupIDs: []string{DefaultPurchaserGroupID}}
	require.NoError(t, store.CreateUser(t.Context(), f.user))
	login, err := f.svc.Login(t.Context(), LoginRequest{Email: email, Password: credentialRacePassword})
	require.NoError(t, err)
	f.session = login.Token
	return f
}

func (f *credentialRaceFixture) newService(store StoreInterface, mail EmailSenderInterface) *Service {
	svc := NewService(ServiceConfig{Store: store, EmailSender: mail, DashboardURL: "https://dashboard.example.com"})
	svc.bcryptCostOverride = 4
	return svc
}

// onRead schedules a concurrent action by a second, unpaused service and
// returns the row as it stood once that action committed.
func (f *credentialRaceFixture) onRead(action func(ctx context.Context, other *Service)) func() *User {
	var after *User
	f.barrier.afterRead = func(ctx context.Context, _ *User) {
		action(ctx, f.newService(f.store, &recordingMailSink{}))
		var err error
		after, err = f.store.GetUserByID(ctx, f.user.ID)
		require.NoError(f.t, err)
	}
	return func() *User {
		require.NotNil(f.t, after, "the concurrent action never ran")
		return after
	}
}

// enrollOnRead lets the victim complete MFA enrollment after the stale read.
func (f *credentialRaceFixture) enrollOnRead() func() *User {
	return f.onRead(func(ctx context.Context, victim *Service) {
		setup, err := victim.MFASetup(ctx, f.user.ID, credentialRacePassword)
		require.NoError(f.t, err)
		codes, err := victim.MFAEnable(ctx, f.user.ID, generateTOTP(setup.Secret, time.Now().Unix()/30))
		require.NoError(f.t, err)
		require.NotEmpty(f.t, codes)
	})
}

func (f *credentialRaceFixture) stored() *User {
	u, err := f.store.GetUserByID(f.t.Context(), f.user.ID)
	require.NoError(f.t, err)
	return u
}

// adminEdit applies edit to the stored row through UpdateUserAdminFields, the
// column-scoped writer an admin's email or active change goes through, so the
// competing write is the one production would make.
func (f *credentialRaceFixture) adminEdit(ctx context.Context, edit func(u *User)) {
	u, err := f.store.GetUserByID(ctx, f.user.ID)
	require.NoError(f.t, err)
	readEmail, readGroupIDs, readActive := u.Email, slices.Clone(u.GroupIDs), u.Active
	edit(u)
	require.NoError(f.t, f.store.UpdateUserAdminFields(ctx, u, readEmail, readGroupIDs, readActive))
}

// requireFactorEnforced proves the victim's factor survived: login without it
// is refused and login with it succeeds.
func (f *credentialRaceFixture) requireFactorEnforced(email, password string, enrolled *User) {
	stored := f.stored()
	require.True(f.t, stored.MFAEnabled, "stale write erased the concurrent MFA enrollment")
	assert.Equal(f.t, enrolled.MFASecret, stored.MFASecret)
	assert.Equal(f.t, enrolled.MFARecoveryCodes, stored.MFARecoveryCodes)
	_, err := f.svc.Login(f.t.Context(), LoginRequest{Email: email, Password: password})
	require.ErrorIs(f.t, err, ErrMFARequired)
	_, err = f.svc.Login(f.t.Context(), LoginRequest{Email: email, Password: password,
		MFACode: generateTOTP(enrolled.MFASecret, time.Now().Unix()/30+1)})
	require.NoError(f.t, err)
}

// assertOnlyChanged asserts every column other than the named ones still
// matches the post-enrollment row.
func assertOnlyChanged(t *testing.T, enrolled, stored *User, mutate func(want *User)) {
	want := *enrolled
	mutate(&want)
	want.UpdatedAt, want.LastLoginAt = stored.UpdatedAt, stored.LastLoginAt
	assert.Equal(t, &want, stored)
}

func TestIntegration_CredentialWritesPreserveConcurrentMFA(t *testing.T) {
	store := NewPostgresStore(setupAuthTestDB(t))
	ctx := t.Context()

	t.Run("profile-email-only", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "profile-email@example.com")
		enrolled := f.enrollOnRead()
		require.NoError(t, f.svc.UpdateUserProfile(ctx, f.user.ID, "profile-email-new@example.com", credentialRacePassword, ""))
		assertOnlyChanged(t, enrolled(), f.stored(), func(w *User) { w.Email = "profile-email-new@example.com" })
		f.requireFactorEnforced("profile-email-new@example.com", credentialRacePassword, enrolled())
	})

	t.Run("profile-email-and-password", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "profile-both@example.com")
		enrolled := f.enrollOnRead()
		require.NoError(t, f.svc.UpdateUserProfile(ctx, f.user.ID, "profile-both-new@example.com", credentialRacePassword, credentialRaceNew))
		stored := f.stored()
		assert.True(t, f.svc.verifyPassword(credentialRaceNew, stored.PasswordHash))
		assert.Equal(t, []string{f.user.PasswordHash}, stored.PasswordHistory)
		assertOnlyChanged(t, enrolled(), stored, func(w *User) {
			w.Email, w.PasswordHash, w.Salt, w.PasswordHistory = "profile-both-new@example.com", stored.PasswordHash, "", stored.PasswordHistory
			w.PasswordVersion++
		})
		_, err := f.svc.ValidateSession(ctx, f.session)
		require.Error(t, err, "a password change must revoke existing sessions")
		f.requireFactorEnforced("profile-both-new@example.com", credentialRaceNew, enrolled())
		_, err = f.svc.Login(ctx, LoginRequest{Email: "profile-both-new@example.com", Password: credentialRacePassword})
		require.EqualError(t, err, genericLoginError)
	})

	t.Run("change-password", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "change-password@example.com")
		enrolled := f.enrollOnRead()
		require.NoError(t, f.svc.ChangePassword(ctx, f.user.ID, ChangePasswordRequest{CurrentPassword: credentialRacePassword, NewPassword: credentialRaceNew}))
		stored := f.stored()
		assert.True(t, f.svc.verifyPassword(credentialRaceNew, stored.PasswordHash))
		assert.Equal(t, []string{f.user.PasswordHash}, stored.PasswordHistory)
		assertOnlyChanged(t, enrolled(), stored, func(w *User) {
			w.PasswordHash, w.Salt, w.PasswordHistory = stored.PasswordHash, "", stored.PasswordHistory
			w.PasswordVersion++
		})
		_, err := f.svc.ValidateSession(ctx, f.session)
		require.Error(t, err, "a password change must revoke existing sessions")
		f.requireFactorEnforced(f.user.Email, credentialRaceNew, enrolled())
		_, err = f.svc.Login(ctx, LoginRequest{Email: f.user.Email, Password: credentialRacePassword})
		require.EqualError(t, err, genericLoginError)
	})

	t.Run("reset-request-with-failing-delivery", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "reset-request@example.com")
		f.mail.err = errors.New("synthetic delivery failure")
		enrolled := f.enrollOnRead()
		require.NoError(t, f.svc.RequestPasswordReset(ctx, f.user.Email))
		assert.Equal(t, []string{f.user.Email}, f.mail.resets)
		stored := f.stored()
		require.NotEmpty(t, stored.PasswordResetToken)
		require.NotNil(t, stored.PasswordResetExpiry)
		assertOnlyChanged(t, enrolled(), stored, func(w *User) {
			w.PasswordResetToken, w.PasswordResetExpiry = stored.PasswordResetToken, stored.PasswordResetExpiry
		})
		f.requireFactorEnforced(f.user.Email, credentialRacePassword, enrolled())

		_, err := store.db.Exec(ctx, "UPDATE users SET password_reset_expiry = NOW() - interval '1 hour' WHERE id = $1", f.user.ID)
		require.NoError(t, err)
		require.NoError(t, f.svc.RequestPasswordReset(ctx, f.user.Email))
		assert.Len(t, f.mail.resets, 2, "a reissue over an expired token must match the stored expiry")
		assert.NotEqual(t, stored.PasswordResetToken, f.stored().PasswordResetToken)
	})
}

func TestIntegration_CredentialWritesRejectStaleCredentials(t *testing.T) {
	store := NewPostgresStore(setupAuthTestDB(t))
	ctx := t.Context()

	t.Run("change-password-after-concurrent-change", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "stale-change@example.com")
		winner := f.onRead(func(ctx context.Context, other *Service) {
			require.NoError(t, other.ChangePassword(ctx, f.user.ID, ChangePasswordRequest{CurrentPassword: credentialRacePassword, NewPassword: "WinnerPassword789!"}))
			login, err := other.Login(ctx, LoginRequest{Email: f.user.Email, Password: "WinnerPassword789!"})
			require.NoError(t, err)
			f.session = login.Token
		})
		err := f.svc.ChangePassword(ctx, f.user.ID, ChangePasswordRequest{CurrentPassword: credentialRacePassword, NewPassword: credentialRaceNew})
		require.ErrorIs(t, err, ErrUserChanged)
		assert.Equal(t, winner(), f.stored())
		_, err = f.svc.ValidateSession(ctx, f.session)
		require.NoError(t, err, "a rejected write must not revoke sessions")
	})

	t.Run("profile-after-concurrent-email-change", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "stale-profile@example.com")
		winner := f.onRead(func(ctx context.Context, _ *Service) {
			f.adminEdit(ctx, func(u *User) { u.Email = "stale-profile-admin@example.com" })
		})
		err := f.svc.UpdateUserProfile(ctx, f.user.ID, "stale-profile-self@example.com", credentialRacePassword, credentialRaceNew)
		require.ErrorIs(t, err, ErrUserChanged)
		assert.Equal(t, winner(), f.stored())
		_, err = f.svc.ValidateSession(ctx, f.session)
		require.NoError(t, err, "a rejected write must not revoke sessions")
	})

	t.Run("reset-after-concurrent-deactivation", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "stale-reset-deactivated@example.com")
		winner := f.onRead(func(ctx context.Context, _ *Service) {
			f.adminEdit(ctx, func(u *User) {
				now := time.Now()
				u.Active, u.DeactivatedAt = false, &now
			})
		})
		require.NoError(t, f.svc.RequestPasswordReset(ctx, f.user.Email))
		assert.Empty(t, f.mail.resets)
		assert.Equal(t, winner(), f.stored())
	})

	t.Run("reset-after-concurrent-email-change", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "stale-reset-email@example.com")
		winner := f.onRead(func(ctx context.Context, _ *Service) {
			f.adminEdit(ctx, func(u *User) { u.Email = "stale-reset-email-new@example.com" })
		})
		require.NoError(t, f.svc.RequestPasswordReset(ctx, f.user.Email))
		assert.Empty(t, f.mail.resets, "a token must not be mailed to the address the user just left")
		stored := f.stored()
		assert.Equal(t, winner(), stored)
		assert.Equal(t, "stale-reset-email-new@example.com", stored.Email)
		assert.Empty(t, stored.PasswordResetToken)
		assert.Nil(t, stored.PasswordResetExpiry)
	})

	t.Run("reset-after-concurrent-reset", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "stale-reset-twice@example.com")
		winner := f.onRead(func(ctx context.Context, other *Service) {
			require.NoError(t, other.RequestPasswordReset(ctx, f.user.Email))
		})
		require.NoError(t, f.svc.RequestPasswordReset(ctx, f.user.Email))
		assert.Empty(t, f.mail.resets, "the losing request must not mail a token it did not store")
		assert.Equal(t, winner(), f.stored())
		assert.NotEmpty(t, f.stored().PasswordResetToken)
	})
}

func (f *credentialRaceFixture) issueResetToken() string {
	token := "reset-token-" + f.user.ID
	_, err := f.store.db.Exec(f.t.Context(), `UPDATE users SET password_reset_token = $2,
		password_reset_expiry = NOW() + interval '1 hour' WHERE id = $1`, f.user.ID, hashSessionToken(token))
	require.NoError(f.t, err)
	return token
}

func TestIntegration_ResetConfirmPreservesConcurrentMFA(t *testing.T) {
	store := NewPostgresStore(setupAuthTestDB(t))
	ctx := t.Context()

	t.Run("confirm", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "reset-confirm@example.com")
		token := f.issueResetToken()
		enrolled := f.enrollOnRead()
		require.NoError(t, f.svc.ConfirmPasswordReset(ctx, PasswordResetConfirm{Token: token, NewPassword: credentialRaceNew}))
		stored := f.stored()
		assert.True(t, f.svc.verifyPassword(credentialRaceNew, stored.PasswordHash))
		assertOnlyChanged(t, enrolled(), stored, func(w *User) {
			w.PasswordHash, w.Salt, w.PasswordHistory = stored.PasswordHash, "", []string{f.user.PasswordHash}
			w.PasswordResetToken, w.PasswordResetExpiry = "", nil
			w.PasswordVersion++
		})
		_, err := f.svc.ValidateSession(ctx, f.session)
		require.Error(t, err, "a reset must revoke existing sessions")
		f.requireFactorEnforced(f.user.Email, credentialRaceNew, enrolled())
		err = f.svc.ConfirmPasswordReset(ctx, PasswordResetConfirm{Token: token, NewPassword: "AnotherPassword789!"})
		require.ErrorContains(t, err, "invalid or expired reset token")
	})

	t.Run("rejected-password-consumes-token", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "reset-confirm-weak@example.com")
		token := f.issueResetToken()
		enrolled := f.enrollOnRead()
		require.Error(t, f.svc.ConfirmPasswordReset(ctx, PasswordResetConfirm{Token: token, NewPassword: "weak"}))
		assertOnlyChanged(t, enrolled(), f.stored(), func(w *User) { w.PasswordResetToken, w.PasswordResetExpiry = "", nil })
		f.requireFactorEnforced(f.user.Email, credentialRacePassword, enrolled())
	})
}

func TestIntegration_ResetConfirmRejectsStaleRead(t *testing.T) {
	store := NewPostgresStore(setupAuthTestDB(t))
	ctx := t.Context()

	t.Run("after-concurrent-confirm", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "reset-twice-confirm@example.com")
		token := f.issueResetToken()
		winner := f.onRead(func(ctx context.Context, other *Service) {
			require.NoError(t, other.ConfirmPasswordReset(ctx, PasswordResetConfirm{Token: token, NewPassword: "WinnerPassword789!"}))
		})
		err := f.svc.ConfirmPasswordReset(ctx, PasswordResetConfirm{Token: token, NewPassword: credentialRaceNew})
		require.ErrorIs(t, err, ErrUserChanged, "a reset token must be spent once")
		assert.Equal(t, winner(), f.stored())
	})

	t.Run("after-concurrent-rejected-confirm", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "reset-after-rejected@example.com")
		token := f.issueResetToken()
		winner := f.onRead(func(ctx context.Context, other *Service) {
			require.Error(t, other.ConfirmPasswordReset(ctx, PasswordResetConfirm{Token: token, NewPassword: "weak"}))
		})
		err := f.svc.ConfirmPasswordReset(ctx, PasswordResetConfirm{Token: token, NewPassword: credentialRaceNew})
		require.ErrorIs(t, err, ErrUserChanged, "a token consumed by a rejected attempt must not set a password")
		assert.Equal(t, winner(), f.stored())
	})

	t.Run("rejected-confirm-after-concurrent-reissue", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "reset-rejected-reissue@example.com")
		token := f.issueResetToken()
		winner := f.onRead(func(ctx context.Context, other *Service) {
			_, err := store.db.Exec(ctx, "UPDATE users SET password_reset_expiry = NOW() - interval '1 hour' WHERE id = $1", f.user.ID)
			require.NoError(t, err)
			require.NoError(t, other.RequestPasswordReset(ctx, f.user.Email))
		})
		err := f.svc.ConfirmPasswordReset(ctx, PasswordResetConfirm{Token: token, NewPassword: "weak"})
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrUserChanged)
		reissued := winner().PasswordResetToken
		require.NotEmpty(t, reissued)
		require.NotEqual(t, hashSessionToken(token), reissued, "the concurrent request must have replaced the token")
		assert.Equal(t, winner(), f.stored(), "a rejected confirm must not clear a token it did not read")
	})

	t.Run("after-concurrent-password-change", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "reset-after-change@example.com")
		token := f.issueResetToken()
		winner := f.onRead(func(ctx context.Context, other *Service) {
			require.NoError(t, other.ChangePassword(ctx, f.user.ID, ChangePasswordRequest{CurrentPassword: credentialRacePassword, NewPassword: "WinnerPassword789!"}))
		})
		err := f.svc.ConfirmPasswordReset(ctx, PasswordResetConfirm{Token: token, NewPassword: credentialRaceNew})
		require.ErrorIs(t, err, ErrUserChanged)
		assert.Equal(t, winner(), f.stored())
		require.ErrorContains(t, f.svc.ConfirmPasswordReset(ctx, PasswordResetConfirm{Token: token, NewPassword: credentialRaceNew}),
			"invalid or expired reset token", "the winning password change must have consumed the token")
		assert.Equal(t, winner(), f.stored())
	})

	t.Run("after-concurrent-deactivation", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "reset-after-deactivation@example.com")
		token := f.issueResetToken()
		winner := f.onRead(func(ctx context.Context, _ *Service) {
			f.adminEdit(ctx, func(u *User) {
				now := time.Now()
				u.Active, u.DeactivatedAt = false, &now
			})
		})
		err := f.svc.ConfirmPasswordReset(ctx, PasswordResetConfirm{Token: token, NewPassword: credentialRaceNew})
		require.ErrorIs(t, err, ErrUserChanged)
		assert.Equal(t, winner(), f.stored())
		require.ErrorIs(t, f.svc.ConfirmPasswordReset(ctx, PasswordResetConfirm{Token: token, NewPassword: credentialRaceNew}), ErrInvalidResetToken)
		assert.Empty(t, f.stored().PasswordResetToken)
	})
}

func (f *credentialRaceFixture) enrollMFA() []string {
	setup, err := f.svc.MFASetup(f.t.Context(), f.user.ID, credentialRacePassword)
	require.NoError(f.t, err)
	// Enroll with the previous step's code: MFAEnable spends its step (#442), and
	// callers then sign in with the current one.
	codes, err := f.svc.MFAEnable(f.t.Context(), f.user.ID, generateTOTP(setup.Secret, time.Now().Unix()/30-1))
	require.NoError(f.t, err)
	return codes
}

func TestIntegration_RecoveryCodeLoginRejectsStaleRead(t *testing.T) {
	store := NewPostgresStore(setupAuthTestDB(t))
	ctx := t.Context()

	t.Run("after-concurrent-use-of-same-code", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "recovery-twice@example.com")
		req := LoginRequest{Email: f.user.Email, Password: credentialRacePassword, MFACode: f.enrollMFA()[0]}
		winner := f.onRead(func(ctx context.Context, other *Service) {
			_, err := other.Login(ctx, req)
			require.NoError(t, err)
		})
		_, err := f.svc.Login(ctx, req)
		require.ErrorIs(t, err, ErrInvalidMFACode, "a recovery code must be spent once")
		assert.Equal(t, winner(), f.stored())
	})

	t.Run("after-concurrent-password-change", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "recovery-after-change@example.com")
		codes := f.enrollMFA()
		winner := f.onRead(func(ctx context.Context, other *Service) {
			require.NoError(t, other.ChangePassword(ctx, f.user.ID, ChangePasswordRequest{CurrentPassword: credentialRacePassword, NewPassword: credentialRaceNew}))
		})
		// Login checks the password it read, so it still succeeds here (a separate, pre-existing gap);
		// this test pins only that the code write leaves the new password in place.
		_, err := f.svc.Login(ctx, LoginRequest{Email: f.user.Email, Password: credentialRacePassword, MFACode: codes[0]})
		require.NoError(t, err)
		stored := f.stored()
		assert.Len(t, stored.MFARecoveryCodes, len(codes)-1)
		assertOnlyChanged(t, winner(), stored, func(w *User) { w.MFARecoveryCodes = stored.MFARecoveryCodes })
		f.requireFactorEnforced(f.user.Email, credentialRaceNew, stored)
	})
}
