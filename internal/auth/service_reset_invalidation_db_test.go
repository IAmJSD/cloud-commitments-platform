//go:build integration

package auth

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegration_CredentialChangeInvalidatesResetToken(t *testing.T) {
	store := NewPostgresStore(setupAuthTestDB(t))
	ctx := t.Context()
	const changed = "ChangedPassword789!"

	cases := map[string]func(f *credentialRaceFixture) error{
		"change-password": func(f *credentialRaceFixture) error {
			return f.svc.ChangePassword(ctx, f.user.ID, ChangePasswordRequest{CurrentPassword: credentialRacePassword, NewPassword: changed})
		},
		"profile-update": func(f *credentialRaceFixture) error {
			return f.svc.UpdateUserProfile(ctx, f.user.ID, "", credentialRacePassword, changed)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newCredentialRaceFixture(t, store, name+"-reset-invalidated@example.com")
			token := f.issueResetToken()
			require.NoError(t, change(f))

			stored := f.stored()
			assert.Empty(t, stored.PasswordResetToken)
			assert.Nil(t, stored.PasswordResetExpiry)

			err := f.svc.ConfirmPasswordReset(ctx, PasswordResetConfirm{Token: token, NewPassword: credentialRaceNew})
			require.ErrorContains(t, err, "invalid or expired reset token")
			assert.True(t, f.svc.verifyPassword(changed, f.stored().PasswordHash), "the newer password must survive")
		})
	}
}

func TestIntegration_CompletePasswordResetEnforcesExpiryInSQL(t *testing.T) {
	store := NewPostgresStore(setupAuthTestDB(t))
	ctx := t.Context()

	t.Run("expired-token-refused-by-the-write", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "reset-expired-sql@example.com")
		token := f.issueResetToken()
		_, err := store.db.Exec(ctx, "UPDATE users SET password_reset_expiry = NOW() - interval '1 minute' WHERE id = $1", f.user.ID)
		require.NoError(t, err)

		read, err := store.GetUserByResetToken(ctx, hashSessionToken(token))
		require.NoError(t, err)
		read.PasswordHash = "replacement-hash"
		err = store.CompletePasswordReset(ctx, read, read.PasswordResetToken, f.user.PasswordHash)
		require.ErrorIs(t, err, ErrUserChanged)
		assert.Equal(t, f.user.PasswordHash, f.stored().PasswordHash)
	})

	t.Run("fresh-token-completes-reset", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "reset-fresh@example.com")
		token := f.issueResetToken()
		require.NoError(t, f.svc.ConfirmPasswordReset(ctx, PasswordResetConfirm{Token: token, NewPassword: credentialRaceNew}))
		stored := f.stored()
		assert.True(t, f.svc.verifyPassword(credentialRaceNew, stored.PasswordHash))
		assert.Empty(t, stored.PasswordResetToken)
	})

	t.Run("stale-password-hash-still-reports-user-changed", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "reset-stale-hash@example.com")
		token := f.issueResetToken()
		read, err := store.GetUserByResetToken(ctx, hashSessionToken(token))
		require.NoError(t, err)
		err = store.CompletePasswordReset(ctx, read, read.PasswordResetToken, "not-the-stored-hash")
		require.ErrorIs(t, err, ErrUserChanged)
	})
}

// inviteMailSink fails every invite send, the case CreateUserResult answers
// by pointing the admin at Forgot Password, and records each reset link.
type inviteMailSink struct {
	resetURLs []string
}

func (m *inviteMailSink) SendPasswordResetEmail(_ context.Context, _, resetURL string) error {
	m.resetURLs = append(m.resetURLs, resetURL)
	return nil
}
func (m *inviteMailSink) SendUserInviteEmail(context.Context, string, string) error {
	return errors.New("smtp unavailable")
}

// TestIntegration_InvitedUserForgotPasswordReissuesLink is audit finding
// A03-009 on real PostgreSQL: an invite whose email failed is recovered via
// Forgot Password, even while the 7-day invite token is still live, and two
// concurrent requests still store and mail exactly one token.
func TestIntegration_InvitedUserForgotPasswordReissuesLink(t *testing.T) {
	db := setupAuthTestDB(t)
	store := NewPostgresStore(db)
	ctx := t.Context()
	newSvc := func(st StoreInterface, mail EmailSenderInterface) *Service {
		svc := NewService(ServiceConfig{Store: st, EmailSender: mail, DashboardURL: "https://dashboard.example.com"})
		svc.bcryptCostOverride = 4
		return svc
	}

	barrier := &credentialReadBarrier{StoreInterface: store}
	mail := &inviteMailSink{}
	svc := newSvc(barrier, mail)
	created, err := svc.CreateUser(ctx, "", CreateUserRequest{Email: "invite-forgot@example.com", GroupIDs: []string{DefaultPurchaserGroupID}})
	require.NoError(t, err)
	require.NotNil(t, created.InviteEmailSent)
	require.False(t, *created.InviteEmailSent, "fixture: the invite email must have failed")
	invited := created.User

	// The invite went out an hour ago: well past the rate limit, with almost
	// all of its 7-day window left.
	_, err = db.Exec(ctx, "UPDATE users SET password_reset_expiry = password_reset_expiry - interval '1 hour' WHERE id = $1", invited.ID)
	require.NoError(t, err)
	inviteHash := invited.PasswordResetToken

	// A second request lands between this one's read and its write.
	otherMail := &inviteMailSink{}
	barrier.afterRead = func(ctx context.Context, _ *User) {
		require.NoError(t, newSvc(store, otherMail).RequestPasswordReset(ctx, invited.Email))
	}
	require.NoError(t, svc.RequestPasswordReset(ctx, invited.Email))

	require.Len(t, otherMail.resetURLs, 1, "the first committed request must issue and mail a link")
	assert.Empty(t, mail.resetURLs, "the losing request must not mail a token it did not store")

	stored, err := store.GetUserByID(ctx, invited.ID)
	require.NoError(t, err)
	assert.False(t, stored.Active)
	assert.NotEqual(t, inviteHash, stored.PasswordResetToken, "the invite token must be replaced")
	require.NotNil(t, stored.PasswordResetExpiry)
	assert.LessOrEqual(t, time.Until(*stored.PasswordResetExpiry), PasswordResetExpiry)

	link, err := url.Parse(otherMail.resetURLs[0])
	require.NoError(t, err)
	token := link.Query().Get("token")
	require.Equal(t, hashSessionToken(token), stored.PasswordResetToken, "the mailed link must be the stored token")

	require.NoError(t, svc.ConfirmPasswordReset(ctx, PasswordResetConfirm{Token: token, NewPassword: credentialRaceNew}))
	activated, err := store.GetUserByID(ctx, invited.ID)
	require.NoError(t, err)
	assert.True(t, activated.Active, "completing the re-sent link must activate the invited account")
	assert.True(t, svc.verifyPassword(credentialRaceNew, activated.PasswordHash))
}
