package auth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #104 (audit finding A03-019): CreateUser, SetupAdmin and the email
// update validated with mail.ParseAddress but stored the raw string, so
// "X <victim@example.com>" passed the uniqueness check against an existing
// victim@example.com row and produced an account that could never log in
// (Login looks up the parsed bare address). Every users.email write now goes
// through normalizeAccountEmail.

var nonBareEmails = []string{
	"Victim <victim@example.com>",
	`"Victim" <victim@example.com>`,
	"<victim@example.com>",
	"victim@example.com (Victim)",
}

func TestNormalizeAccountEmail(t *testing.T) {
	for _, raw := range nonBareEmails {
		_, err := normalizeAccountEmail(raw)
		assert.ErrorIs(t, err, ErrInvalidEmail, "%q must be refused", raw)
	}
	for _, raw := range []string{"", "victim", "victim@", "a@b@example.com"} {
		_, err := normalizeAccountEmail(raw)
		assert.ErrorIs(t, err, ErrInvalidEmail, "%q must be refused", raw)
	}
	for raw, want := range map[string]string{
		"victim@example.com":       "victim@example.com",
		"  victim@example.com\t":   "victim@example.com",
		"Victim.Name@Example.com":  "Victim.Name@Example.com",
		"first+tag@sub.example.io": "first+tag@sub.example.io",
	} {
		got, err := normalizeAccountEmail(raw)
		require.NoError(t, err, "%q", raw)
		assert.Equal(t, want, got, "%q", raw)
	}
}

func TestCreateUser_StoresBareEmail(t *testing.T) {
	ctx := context.Background()

	for _, raw := range nonBareEmails {
		t.Run("refuses "+raw, func(t *testing.T) {
			mockStore := new(MockStore)
			t.Cleanup(func() { mockStore.AssertExpectations(t) })
			service := createTestService(mockStore, new(MockEmailSender))

			_, err := service.CreateUser(ctx, "", CreateUserRequest{
				Email: raw, Password: "SecurePass@123", GroupIDs: []string{DefaultAdminGroupID},
			})
			require.ErrorIs(t, err, ErrInvalidEmail)
			mockStore.AssertNotCalled(t, "CreateUser", mock.Anything, mock.Anything)
		})
	}

	t.Run("checks uniqueness on and stores the trimmed address", func(t *testing.T) {
		mockStore := new(MockStore)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		service := createTestService(mockStore, new(MockEmailSender))

		var stored *User
		mockStore.On("GetUserByEmail", ctx, "new@example.com").Return(nil, nil).Once()
		mockStore.On("CreateUser", ctx, mock.AnythingOfType("*auth.User")).
			Run(func(args mock.Arguments) { stored = args.Get(1).(*User) }).
			Return(nil).Once()

		_, err := service.CreateUser(ctx, "", CreateUserRequest{
			Email: " new@example.com ", Password: "SecurePass@123", GroupIDs: []string{DefaultAdminGroupID},
		})
		require.NoError(t, err)
		require.NotNil(t, stored)
		assert.Equal(t, "new@example.com", stored.Email)
	})
}

func TestSetupAdmin_StoresBareEmail(t *testing.T) {
	ctx := context.Background()

	for _, raw := range nonBareEmails {
		t.Run("refuses "+raw, func(t *testing.T) {
			mockStore := new(MockStore)
			t.Cleanup(func() { mockStore.AssertExpectations(t) })
			service := createTestService(mockStore, new(MockEmailSender))
			mockStore.On("AdminExists", ctx).Return(false, nil).Once()

			_, err := service.SetupAdmin(ctx, SetupAdminRequest{Email: raw, Password: "SecurePass@123"})
			require.ErrorIs(t, err, ErrInvalidEmail)
			mockStore.AssertNotCalled(t, "CreateAdminIfNone", mock.Anything, mock.Anything)
		})
	}

	t.Run("stores the trimmed address", func(t *testing.T) {
		mockStore := new(MockStore)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		service := createTestService(mockStore, new(MockEmailSender))

		var stored *User
		mockStore.On("AdminExists", ctx).Return(false, nil).Once()
		mockStore.On("CreateAdminIfNone", ctx, mock.AnythingOfType("*auth.User")).
			Run(func(args mock.Arguments) { stored = args.Get(1).(*User) }).
			Return(true, nil).Once()
		mockStore.On("CreateSession", ctx, mock.AnythingOfType("*auth.Session")).Return(nil).Once()

		resp, err := service.SetupAdmin(ctx, SetupAdminRequest{Email: "admin@example.com\n", Password: "SecurePass@123"})
		require.NoError(t, err)
		require.NotNil(t, stored)
		assert.Equal(t, "admin@example.com", stored.Email)
		assert.Equal(t, "admin@example.com", resp.User.Email)
	})
}

func TestUpdateUserProfile_StoresBareEmail(t *testing.T) {
	ctx := context.Background()
	hash := TestPasswordHash(t, "OldPassword123")
	newUser := func(email string) *User {
		return &User{ID: "user-123", Email: email, PasswordHash: hash, Active: true, CreatedAt: time.Now()}
	}

	for _, raw := range nonBareEmails {
		t.Run("refuses "+raw, func(t *testing.T) {
			mockStore := new(MockStore)
			t.Cleanup(func() { mockStore.AssertExpectations(t) })
			service := createTestService(mockStore, new(MockEmailSender))
			mockStore.On("GetUserByID", ctx, "user-123").Return(newUser("old@example.com"), nil).Once()

			err := service.UpdateUserProfile(ctx, "user-123", raw, "OldPassword123", "")
			require.ErrorIs(t, err, ErrInvalidEmail)
			mockStore.AssertNotCalled(t, "UpdateUserCredentials", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}

	t.Run("checks uniqueness on and stores the trimmed address", func(t *testing.T) {
		mockStore := new(MockStore)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		service := createTestService(mockStore, new(MockEmailSender))

		var stored *User
		mockStore.On("GetUserByID", ctx, "user-123").Return(newUser("old@example.com"), nil).Once()
		mockStore.On("GetUserByEmail", ctx, "new@example.com").Return(nil, nil).Once()
		mockStore.On("UpdateUserCredentials", ctx, mock.AnythingOfType("*auth.User"), "old@example.com", hash).
			Run(func(args mock.Arguments) { stored = args.Get(1).(*User) }).
			Return(nil).Once()

		require.NoError(t, service.UpdateUserProfile(ctx, "user-123", "\tnew@example.com ", "OldPassword123", ""))
		require.NotNil(t, stored)
		assert.Equal(t, "new@example.com", stored.Email)
	})

	t.Run("resubmitting a stored non-bare address is still a no-op", func(t *testing.T) {
		// A row written before this fix may hold a display-name form. A
		// profile save that resubmits it unchanged (e.g. a password change)
		// must not start failing.
		mockStore := new(MockStore)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		service := createTestService(mockStore, new(MockEmailSender))

		legacy := "Old <old@example.com>"
		mockStore.On("GetUserByID", ctx, "user-123").Return(newUser(legacy), nil).Once()
		mockStore.On("UpdateUserCredentials", ctx, mock.AnythingOfType("*auth.User"), legacy, hash).Return(nil).Once()

		require.NoError(t, service.UpdateUserProfile(ctx, "user-123", legacy, "OldPassword123", ""))
	})
}
