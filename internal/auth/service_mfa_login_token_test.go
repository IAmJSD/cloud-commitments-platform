package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #134: unit coverage of the mfa_token login form against MockStore.
// The real-Postgres behavior (expiry, single use, attempt cap under
// concurrency) is in service_mfa_login_token_db_test.go.

const mfaTokenUnitSecret = "JBSWY3DPEHPK3PXP"

func mfaTokenUnitUser(t *testing.T) *User {
	t.Helper()
	u := createTestUser(t, "SecurePass@123")
	u.MFAEnabled = true
	u.MFASecret = mfaTokenUnitSecret
	u.PasswordVersion = 3
	return u
}

func mfaTokenUnitReq(code string) LoginRequest {
	return LoginRequest{Email: "test@example.com", MFAToken: "raw-token", MFACode: code}
}

func currentUnitTOTP() string {
	return generateTOTP(mfaTokenUnitSecret, time.Now().Unix()/30)
}

func TestLoginWithMFAToken_HappyPath(t *testing.T) {
	ctx := context.Background()
	store := new(MockStore)
	svc := createTestService(store, new(MockEmailSender))
	user := mfaTokenUnitUser(t)
	hash := hashSessionToken("raw-token")

	store.On("ClaimMFALoginTokenAttempt", ctx, hash, MaxMFALoginTokenAttempts).
		Return(&MFALoginToken{UserID: user.ID, PasswordVersion: 3, Attempts: 1}, nil).Once()
	store.On("GetUserByEmail", ctx, "test@example.com").Return(user, nil).Once()
	store.On("ClaimTOTPCounter", ctx, user.ID, mock.AnythingOfType("int64")).Return(true, nil).Once()
	store.On("ConsumeMFALoginToken", ctx, hash).Return(true, nil).Once()
	store.On("CreateSession", ctx, mock.Anything).Return(nil).Once()
	store.On("RecordSuccessfulLogin", ctx, user.ID).Return(nil).Once()

	resp, err := svc.Login(ctx, mfaTokenUnitReq(currentUnitTOTP()))
	require.NoError(t, err)
	assert.NotEmpty(t, resp.Token)
	store.AssertExpectations(t)
}

// A token that the store does not hand out (unknown, expired, used, out of
// attempts) stops the login before any user lookup or code check.
func TestLoginWithMFAToken_UnclaimableTokenRefused(t *testing.T) {
	ctx := context.Background()
	store := new(MockStore)
	svc := createTestService(store, new(MockEmailSender))

	store.On("ClaimMFALoginTokenAttempt", ctx, hashSessionToken("raw-token"), MaxMFALoginTokenAttempts).
		Return(nil, nil).Once()

	_, err := svc.Login(ctx, mfaTokenUnitReq(currentUnitTOTP()))
	require.ErrorIs(t, err, ErrInvalidMFAToken)
	store.AssertExpectations(t)
	store.AssertNotCalled(t, "GetUserByEmail", mock.Anything, mock.Anything)
	store.AssertNotCalled(t, "ClaimTOTPCounter", mock.Anything, mock.Anything, mock.Anything)
}

func TestLoginWithMFAToken_ClaimErrorRefused(t *testing.T) {
	ctx := context.Background()
	store := new(MockStore)
	svc := createTestService(store, new(MockEmailSender))
	store.On("ClaimMFALoginTokenAttempt", ctx, mock.Anything, MaxMFALoginTokenAttempts).
		Return(nil, errors.New("db down")).Once()

	_, err := svc.Login(ctx, mfaTokenUnitReq(currentUnitTOTP()))
	require.ErrorIs(t, err, ErrInvalidMFAToken)
	store.AssertExpectations(t)
}

// A body mixing a password with the token, or a token without a code, is
// refused without spending an attempt.
func TestLoginWithMFAToken_MalformedFormRefused(t *testing.T) {
	ctx := context.Background()
	for name, req := range map[string]LoginRequest{
		"password-and-token": {Email: "test@example.com", Password: "SecurePass@123", MFAToken: "raw-token", MFACode: "123456"},
		"no-code":            {Email: "test@example.com", MFAToken: "raw-token"},
	} {
		t.Run(name, func(t *testing.T) {
			store := new(MockStore)
			svc := createTestService(store, new(MockEmailSender))
			_, err := svc.Login(ctx, req)
			require.ErrorIs(t, err, ErrInvalidMFAToken)
			store.AssertNotCalled(t, "ClaimMFALoginTokenAttempt", mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

// The token is bound to its user, to that user's password version and to MFA
// still being on. Each mismatch is refused before any code is checked.
func TestLoginWithMFAToken_BindingMismatchRefused(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		claim  MFALoginToken
		mutate func(u *User)
	}{
		"other-user":       {claim: MFALoginToken{UserID: "someone-else", PasswordVersion: 3}},
		"password-changed": {claim: MFALoginToken{UserID: "user-123", PasswordVersion: 2}},
		"mfa-disabled":     {claim: MFALoginToken{UserID: "user-123", PasswordVersion: 3}, mutate: func(u *User) { u.MFAEnabled = false }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := new(MockStore)
			svc := createTestService(store, new(MockEmailSender))
			user := mfaTokenUnitUser(t)
			if tc.mutate != nil {
				tc.mutate(user)
			}
			claim := tc.claim
			store.On("ClaimMFALoginTokenAttempt", ctx, mock.Anything, MaxMFALoginTokenAttempts).Return(&claim, nil).Once()
			store.On("GetUserByEmail", ctx, "test@example.com").Return(user, nil).Once()

			_, err := svc.Login(ctx, mfaTokenUnitReq(currentUnitTOTP()))
			require.ErrorIs(t, err, ErrInvalidMFAToken)
			store.AssertExpectations(t)
			store.AssertNotCalled(t, "ClaimTOTPCounter", mock.Anything, mock.Anything, mock.Anything)
			store.AssertNotCalled(t, "CreateSession", mock.Anything, mock.Anything)
		})
	}
}

// A locked or deactivated account is refused with the generic login error.
func TestLoginWithMFAToken_LockedAccountRefused(t *testing.T) {
	ctx := context.Background()
	store := new(MockStore)
	svc := createTestService(store, new(MockEmailSender))
	user := mfaTokenUnitUser(t)
	until := time.Now().Add(time.Minute)
	user.LockedUntil = &until
	store.On("ClaimMFALoginTokenAttempt", ctx, mock.Anything, MaxMFALoginTokenAttempts).
		Return(&MFALoginToken{UserID: user.ID, PasswordVersion: 3}, nil).Once()
	store.On("GetUserByEmail", ctx, "test@example.com").Return(user, nil).Once()

	_, err := svc.Login(ctx, mfaTokenUnitReq(currentUnitTOTP()))
	require.EqualError(t, err, genericLoginError)
	store.AssertNotCalled(t, "ClaimTOTPCounter", mock.Anything, mock.Anything, mock.Anything)
}

// A wrong code counts toward the account lockout, as on the password form,
// and leaves the token in place for the remaining attempts.
func TestLoginWithMFAToken_WrongCodeRecordsFailure(t *testing.T) {
	ctx := context.Background()
	store := new(MockStore)
	svc := createTestService(store, new(MockEmailSender))
	user := mfaTokenUnitUser(t)
	store.On("ClaimMFALoginTokenAttempt", ctx, mock.Anything, MaxMFALoginTokenAttempts).
		Return(&MFALoginToken{UserID: user.ID, PasswordVersion: 3}, nil).Once()
	store.On("GetUserByEmail", ctx, "test@example.com").Return(user, nil).Once()
	store.On("RecordFailedLogin", ctx, user.ID).Return(nil).Once()

	_, err := svc.Login(ctx, mfaTokenUnitReq("not-a-code"))
	require.ErrorIs(t, err, ErrInvalidMFACode)
	store.AssertExpectations(t)
	store.AssertNotCalled(t, "ConsumeMFALoginToken", mock.Anything, mock.Anything)
}

// Losing the consume race (another request already used the token) means no
// session, even though the code was right.
func TestLoginWithMFAToken_LostConsumeRefused(t *testing.T) {
	ctx := context.Background()
	store := new(MockStore)
	svc := createTestService(store, new(MockEmailSender))
	user := mfaTokenUnitUser(t)
	store.On("ClaimMFALoginTokenAttempt", ctx, mock.Anything, MaxMFALoginTokenAttempts).
		Return(&MFALoginToken{UserID: user.ID, PasswordVersion: 3}, nil).Once()
	store.On("GetUserByEmail", ctx, "test@example.com").Return(user, nil).Once()
	store.On("ClaimTOTPCounter", ctx, user.ID, mock.AnythingOfType("int64")).Return(true, nil).Once()
	store.On("ConsumeMFALoginToken", ctx, mock.Anything).Return(false, nil).Once()

	_, err := svc.Login(ctx, mfaTokenUnitReq(currentUnitTOTP()))
	require.ErrorIs(t, err, ErrInvalidMFAToken)
	store.AssertNotCalled(t, "CreateSession", mock.Anything, mock.Anything)
}
