package auth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestHelperFunctions(t *testing.T) {
	t.Run("generateToken returns unique values", func(t *testing.T) {
		token1, err := generateToken()
		require.NoError(t, err)
		assert.NotEmpty(t, token1)

		token2, err := generateToken()
		require.NoError(t, err)
		assert.NotEqual(t, token1, token2, "tokens should be unique")
	})

	t.Run("password validation", func(t *testing.T) {
		service := &Service{}

		// Valid password
		err := service.validatePassword("SecurePass@123")
		assert.NoError(t, err)

		// Too short
		err = service.validatePassword("Short1")
		assert.Error(t, err)

		// No uppercase
		err = service.validatePassword("lowercase123")
		assert.Error(t, err)

		// No lowercase
		err = service.validatePassword("UPPERCASE123")
		assert.Error(t, err)

		// No number
		err = service.validatePassword("NoNumberHere")
		assert.Error(t, err)

		// Empty password
		err = service.validatePassword("")
		assert.Error(t, err)
	})

	t.Run("password hashing and verification", func(t *testing.T) {
		service := newTestService()
		password := "TestPassword123"

		hash, err := service.hashPassword(password)
		require.NoError(t, err)
		assert.NotEmpty(t, hash)

		// Verify correct password
		assert.True(t, service.verifyPassword(password, hash))

		// Verify wrong password
		assert.False(t, service.verifyPassword("wrongpassword", hash))
	})
}

func TestContainsAll(t *testing.T) {
	t.Run("returns false when only some requested values are allowed", func(t *testing.T) {
		allowed := []string{"a", "b", "c"}
		requested := []string{"b", "d"}
		assert.False(t, containsAll(allowed, requested))
	})

	t.Run("returns false when no intersection", func(t *testing.T) {
		allowed := []string{"a", "b", "c"}
		requested := []string{"d", "e"}
		assert.False(t, containsAll(allowed, requested))
	})

	t.Run("returns false when allowed is empty", func(t *testing.T) {
		allowed := []string{}
		requested := []string{"a", "b"}
		assert.False(t, containsAll(allowed, requested))
	})

	t.Run("fails closed when requested is empty", func(t *testing.T) {
		allowed := []string{"a", "b"}
		assert.False(t, containsAll(allowed, []string{}))
		assert.False(t, containsAll(allowed, nil))
	})

	t.Run("returns true when every requested value is allowed", func(t *testing.T) {
		allowed := []string{"a", "b", "c"}
		requested := []string{"a", "b"}
		assert.True(t, containsAll(allowed, requested))
	})

	t.Run("comparison is exact", func(t *testing.T) {
		assert.False(t, containsAll([]string{"A"}, []string{"a"}))
		assert.False(t, containsAll([]string{"a "}, []string{"a"}))
	})
}

// A session records the password version of the user row its login verified
// (issue #398).
func TestCreateSession_RecordsPasswordVersion(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	service := createTestService(mockStore, new(MockEmailSender))

	var stored *Session
	mockStore.On("CreateSession", ctx, mock.AnythingOfType("*auth.Session")).
		Run(func(args mock.Arguments) { stored = args.Get(1).(*Session) }).Return(nil).Once()

	_, err := service.createSession(ctx, &User{ID: "user-123", Email: "test@example.com", PasswordVersion: 3}, "", "")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, int64(3), stored.PasswordVersion)
	mockStore.AssertExpectations(t)
}

// ValidateSession rejects a session issued under another password version,
// whether or not the best-effort session delete ran (issue #398).
func TestValidateSession_PasswordVersion(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name           string
		sessionVersion int64
		userVersion    int64
		wantErr        error
	}{
		{name: "same version", sessionVersion: 2, userVersion: 2},
		{name: "password changed after login", sessionVersion: 1, userVersion: 2, wantErr: ErrSessionPasswordRotated},
		{name: "session newer than the user row", sessionVersion: 3, userVersion: 2, wantErr: ErrSessionPasswordRotated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mockStore := new(MockStore)
			service := createTestService(mockStore, new(MockEmailSender))
			hashed := hashSessionToken("raw-token")
			mockStore.On("GetSession", ctx, hashed).Return(&Session{
				Token: hashed, UserID: "user-123", ExpiresAt: time.Now().Add(time.Hour), PasswordVersion: tc.sessionVersion,
			}, nil).Once()
			mockStore.On("GetUserByID", ctx, "user-123").
				Return(&User{ID: "user-123", Active: true, PasswordVersion: tc.userVersion}, nil).Once()

			session, err := service.ValidateSession(ctx, "raw-token")
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				assert.Nil(t, session)
			} else {
				require.NoError(t, err)
				assert.Equal(t, "raw-token", session.Token)
			}
			mockStore.AssertExpectations(t)
		})
	}
}
