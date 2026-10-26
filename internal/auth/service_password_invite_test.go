package auth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Audit finding A03-009 (issue #263): an invite token is issued with
// PasswordSetupExpiry (7d), but the forgot-password rate limit derived the
// token's age as if every token had the 1h PasswordResetExpiry. For an invite
// that age came out negative for almost the whole week, so the documented
// "re-mail the setup link via Forgot Password" recovery returned success
// without issuing a token or sending mail.
//
// The refusal case stubs NO write or email expectation: testify panics if
// either lands, so the assertion cannot be vacuous.

// invitedUser is an account created through the invite flow and not yet
// activated: inactive, never deactivated, holding the invite token.
func invitedUser(issuedAgo time.Duration) *User {
	expiry := time.Now().Add(PasswordSetupExpiry - issuedAgo)
	return &User{
		ID:                  "user-invited",
		Email:               "invited@example.com",
		PasswordResetToken:  "invite-token-hash",
		PasswordResetExpiry: &expiry,
		Active:              false,
	}
}

func TestService_RequestPasswordReset_InvitedUser(t *testing.T) {
	ctx := context.Background()

	t.Run("invite older than the rate limit gets a fresh link", func(t *testing.T) {
		mockStore := new(MockStore)
		mockEmail := new(MockEmailSender)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		t.Cleanup(func() { mockEmail.AssertExpectations(t) })
		svc := createTestService(mockStore, mockEmail)

		user := invitedUser(time.Hour)
		inviteExpiry := *user.PasswordResetExpiry

		mockStore.On("GetUserByEmail", ctx, user.Email).Return(user, nil).Once()
		mockStore.On("SetPasswordResetToken", ctx, mock.MatchedBy(func(u *User) bool {
			// A normal 1h reset token replaces the invite token.
			return u.ID == user.ID && u.PasswordResetToken != "invite-token-hash" &&
				u.PasswordResetExpiry != nil &&
				time.Until(*u.PasswordResetExpiry) <= PasswordResetExpiry
		}), mock.MatchedBy(func(readExpiry *time.Time) bool {
			// The write is conditioned on the invite row it read.
			return readExpiry != nil && readExpiry.Equal(inviteExpiry)
		})).Return(nil).Once()
		mockEmail.On("SendPasswordResetEmail", ctx, user.Email, mock.AnythingOfType("string")).Return(nil).Once()

		require.NoError(t, svc.RequestPasswordReset(ctx, user.Email))
	})

	t.Run("invite inside the rate limit window is not reissued", func(t *testing.T) {
		mockStore := new(MockStore)
		mockEmail := new(MockEmailSender)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		t.Cleanup(func() { mockEmail.AssertExpectations(t) })
		svc := createTestService(mockStore, mockEmail)

		user := invitedUser(PasswordResetRateLimit / 2)
		mockStore.On("GetUserByEmail", ctx, user.Email).Return(user, nil).Once()
		// SetPasswordResetToken and SendPasswordResetEmail must NOT be called.

		require.NoError(t, svc.RequestPasswordReset(ctx, user.Email))
	})
}

// TestResetRecentlyIssued pins the age derivation for both token kinds at the
// rate-limit boundary, so neither a reset token nor an invite token can drift
// to "always limited" or "never limited".
func TestResetRecentlyIssued(t *testing.T) {
	cases := []struct {
		name      string
		lifetime  time.Duration
		issuedAgo time.Duration
		want      bool
	}{
		{"reset token just issued", PasswordResetExpiry, 5 * time.Second, true},
		{"reset token past the limit", PasswordResetExpiry, PasswordResetRateLimit + 5*time.Second, false},
		{"invite token just issued", PasswordSetupExpiry, 5 * time.Second, true},
		{"invite token past the limit", PasswordSetupExpiry, PasswordResetRateLimit + 5*time.Second, false},
		{"invite token a day old", PasswordSetupExpiry, 24 * time.Hour, false},
		// Just over PasswordResetExpiry left is still an invite (aged ~7d), not a
		// reset token issued in the future, so it is not limited.
		{"invite token about to expire", PasswordSetupExpiry, PasswordSetupExpiry - PasswordResetExpiry - 30*time.Second, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expiry := time.Now().Add(tc.lifetime - tc.issuedAgo)
			user := &User{PasswordResetExpiry: &expiry}
			assert.Equal(t, tc.want, resetRecentlyIssued(user, "u@example.com"))
		})
	}

	t.Run("no token is never limited", func(t *testing.T) {
		assert.False(t, resetRecentlyIssued(&User{}, "u@example.com"))
	})
}
