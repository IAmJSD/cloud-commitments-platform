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
	store.On("GetUserByID", ctx, user.ID).Return(user, nil).Once()
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
	store.AssertNotCalled(t, "GetUserByID", mock.Anything, mock.Anything)
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
// still being on. Each mismatch is refused before any code is checked. The
// user is read by the token's user ID.
func TestLoginWithMFAToken_BindingMismatchRefused(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		claim  MFALoginToken
		mutate func(u *User)
	}{
		"other-user": {claim: MFALoginToken{UserID: "someone-else", PasswordVersion: 3}, mutate: func(u *User) {
			u.ID, u.Email = "someone-else", "someone-else@example.com"
		}},
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
			store.On("GetUserByID", ctx, user.ID).Return(user, nil).Once()

			_, err := svc.Login(ctx, mfaTokenUnitReq(currentUnitTOTP()))
			require.ErrorIs(t, err, ErrInvalidMFAToken)
			store.AssertExpectations(t)
			store.AssertNotCalled(t, "ClaimTOTPCounter", mock.Anything, mock.Anything, mock.Anything)
			store.AssertNotCalled(t, "CreateSession", mock.Anything, mock.Anything)
		})
	}
}

// Whatever the email names (no account, an inactive account, another active
// account) and whatever state the token's own user is in (inactive, locked),
// a token that does not lead to a sign-in gets one answer: ErrInvalidMFAToken,
// with no code checked. The submitted email is never looked up.
func TestLoginWithMFAToken_EveryMismatchSameAnswer(t *testing.T) {
	ctx := context.Background()
	lockedUntil := time.Now().Add(time.Minute)
	cases := map[string]struct {
		email  string
		mutate func(u *User)
	}{
		"non-existent email":  {email: "nobody@example.com"},
		"inactive user email": {email: "inactive@example.com"},
		"other user email":    {email: "other@example.com"},
		"token user inactive": {email: "test@example.com", mutate: func(u *User) { u.Active = false }},
		"token user locked":   {email: "test@example.com", mutate: func(u *User) { u.LockedUntil = &lockedUntil }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := new(MockStore)
			svc := createTestService(store, new(MockEmailSender))
			user := mfaTokenUnitUser(t)
			if tc.mutate != nil {
				tc.mutate(user)
			}
			store.On("ClaimMFALoginTokenAttempt", ctx, mock.Anything, MaxMFALoginTokenAttempts).
				Return(&MFALoginToken{UserID: user.ID, PasswordVersion: 3}, nil).Once()
			store.On("GetUserByID", ctx, user.ID).Return(user, nil).Maybe()
			// What a lookup by the submitted email would find; it must not
			// be asked.
			store.On("GetUserByEmail", ctx, tc.email).Return(byEmail(user, tc.email), nil).Maybe()

			req := mfaTokenUnitReq(currentUnitTOTP())
			req.Email = tc.email
			_, err := svc.Login(ctx, req)
			require.ErrorIs(t, err, ErrInvalidMFAToken)
			store.AssertExpectations(t)
			store.AssertCalled(t, "GetUserByID", ctx, user.ID)
			store.AssertNotCalled(t, "GetUserByEmail", mock.Anything, mock.Anything)
			store.AssertNotCalled(t, "ClaimTOTPCounter", mock.Anything, mock.Anything, mock.Anything)
			store.AssertNotCalled(t, "RecordFailedLogin", mock.Anything, mock.Anything)
		})
	}
}

// byEmail returns the account the unit tests' store holds under email:
// tokenUser, an inactive or an active other user, or none.
func byEmail(tokenUser *User, email string) *User {
	switch email {
	case tokenUser.Email:
		return tokenUser
	case "inactive@example.com":
		return &User{ID: "user-inactive", Email: email, MFAEnabled: true, PasswordVersion: 3}
	case "other@example.com":
		return &User{ID: "user-other", Email: email, Active: true, MFAEnabled: true, PasswordVersion: 3}
	}
	return nil
}

// A failed lookup of the token's user is refused the same way.
func TestLoginWithMFAToken_UserLookupErrorRefused(t *testing.T) {
	ctx := context.Background()
	for name, ret := range map[string]struct {
		user *User
		err  error
	}{
		"store error": {err: errors.New("db down")},
		"user gone":   {},
	} {
		t.Run(name, func(t *testing.T) {
			store := new(MockStore)
			svc := createTestService(store, new(MockEmailSender))
			store.On("ClaimMFALoginTokenAttempt", ctx, mock.Anything, MaxMFALoginTokenAttempts).
				Return(&MFALoginToken{UserID: "user-123", PasswordVersion: 3}, nil).Once()
			store.On("GetUserByID", ctx, "user-123").Return(ret.user, ret.err).Once()

			_, err := svc.Login(ctx, mfaTokenUnitReq(currentUnitTOTP()))
			require.ErrorIs(t, err, ErrInvalidMFAToken)
			store.AssertExpectations(t)
		})
	}
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
	store.On("GetUserByID", ctx, user.ID).Return(user, nil).Once()
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
	t.Cleanup(func() { store.AssertExpectations(t) })
	svc := createTestService(store, new(MockEmailSender))
	user := mfaTokenUnitUser(t)
	store.On("ClaimMFALoginTokenAttempt", ctx, mock.Anything, MaxMFALoginTokenAttempts).
		Return(&MFALoginToken{UserID: user.ID, PasswordVersion: 3}, nil).Once()
	store.On("GetUserByID", ctx, user.ID).Return(user, nil).Once()
	store.On("ClaimTOTPCounter", ctx, user.ID, mock.AnythingOfType("int64")).Return(true, nil).Once()
	store.On("ConsumeMFALoginToken", ctx, mock.Anything).Return(false, nil).Once()

	_, err := svc.Login(ctx, mfaTokenUnitReq(currentUnitTOTP()))
	require.ErrorIs(t, err, ErrInvalidMFAToken)
	store.AssertNotCalled(t, "CreateSession", mock.Anything, mock.Anything)
}
