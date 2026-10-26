package auth

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func TestLogin_AccountLockout_Bookkeeping(t *testing.T) {
	for _, tc := range []struct {
		name       string
		attempts   int
		lockOffset time.Duration
		password   string
		mfa        bool
		method     string
	}{
		{name: "locked", attempts: 5, lockOffset: time.Minute, password: "CorrectPassword123"},
		{name: "below threshold", attempts: 2, password: "wrong", method: "RecordFailedLogin"},
		{name: "threshold", attempts: 4, password: "wrong", method: "RecordFailedLogin"},
		{name: "expired failure", attempts: 5, lockOffset: -time.Minute, password: "wrong", method: "RecordFailedLogin"},
		{name: "expired success", attempts: 5, lockOffset: -time.Minute, password: "CorrectPassword123", method: "RecordSuccessfulLogin"},
		{name: "reset on success", attempts: 3, password: "CorrectPassword123", method: "RecordSuccessfulLogin"},
		{name: "MFA failure", attempts: 4, password: "CorrectPassword123", mfa: true, method: "RecordFailedLogin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := new(MockStore)
			service := createTestService(store, new(MockEmailSender))
			user := createTestUser(t, "CorrectPassword123")
			user.FailedLoginAttempts = tc.attempts
			if tc.lockOffset != 0 {
				until := time.Now().Add(tc.lockOffset)
				user.LockedUntil = &until
			}
			user.MFAEnabled = tc.mfa
			user.MFASecret = "JBSWY3DPEHPK3PXP"
			store.On("GetUserByEmail", ctx, user.Email).Return(user, nil).Once()
			if tc.method != "" {
				store.On(tc.method, ctx, user.ID).Return(nil).Once()
			}
			if tc.method == "RecordSuccessfulLogin" {
				store.On("CreateSession", ctx, mock.AnythingOfType("*auth.Session")).Return(nil).Once()
			}
			response, err := service.Login(ctx, LoginRequest{Email: user.Email, Password: tc.password, MFACode: "invalid"})
			if tc.method == "RecordSuccessfulLogin" {
				require.NoError(t, err)
				require.NotEmpty(t, response.Token)
			} else {
				require.Error(t, err)
				assert.Nil(t, response)
				if tc.mfa {
					assert.ErrorIs(t, err, ErrInvalidMFACode)
				} else {
					assert.EqualError(t, err, genericLoginError)
				}
			}
			store.AssertExpectations(t)
		})
	}
}

func TestLogin_BookkeepingErrors(t *testing.T) {
	for _, tc := range []struct {
		name, password, method string
		sessionError           bool
	}{
		{name: "failure write", password: "wrong", method: "RecordFailedLogin"},
		{name: "success write", password: "CorrectPassword123", method: "RecordSuccessfulLogin"},
		{name: "session creation", password: "CorrectPassword123", sessionError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := new(MockStore)
			service := createTestService(store, new(MockEmailSender))
			user := createTestUser(t, "CorrectPassword123")
			store.On("GetUserByEmail", ctx, user.Email).Return(user, nil).Once()
			if tc.method != "" {
				store.On(tc.method, ctx, user.ID).Return(assert.AnError).Once()
			}
			var issued *Session
			if tc.password == "CorrectPassword123" {
				var sessionErr error
				if tc.sessionError {
					sessionErr = assert.AnError
				}
				store.On("CreateSession", ctx, mock.AnythingOfType("*auth.Session")).Run(func(args mock.Arguments) { issued = args.Get(1).(*Session) }).Return(sessionErr).Once()
			}
			response, err := service.Login(ctx, LoginRequest{Email: user.Email, Password: tc.password})
			if tc.method == "RecordSuccessfulLogin" {
				require.NoError(t, err)
				require.NotNil(t, response)
				require.NotNil(t, issued)
				store.On("GetSession", ctx, hashSessionToken(response.Token)).Return(issued, nil).Once()
				store.On("GetUserByID", ctx, user.ID).Return(user, nil).Once()
				_, err = service.ValidateSession(ctx, response.Token)
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Nil(t, response)
				if tc.method == "RecordFailedLogin" {
					assert.EqualError(t, err, genericLoginError)
				}
			}
			store.AssertExpectations(t)
		})
	}
}

// Production (NewService with no BcryptCost) compares against the fixed
// cost-12 sentinel. Only a test cost override switches the timing-equalizing
// compare to a hash at that cost.
func TestDummyHashForCompare_MatchesHashCost(t *testing.T) {
	prod := NewService(ServiceConfig{DashboardURL: "https://dashboard.example.com", CSRFKey: testCSRFKey}).dummyHashForCompare()
	assert.Equal(t, dummyPasswordHash, prod)
	prodCost, err := bcrypt.Cost([]byte(prod))
	require.NoError(t, err)
	assert.Equal(t, bcryptCost, prodCost)

	for name, svc := range map[string]*Service{
		"override": newTestService(),
		"config":   NewService(ServiceConfig{DashboardURL: "https://dashboard.example.com", CSRFKey: testCSRFKey, BcryptCost: bcrypt.MinCost}),
	} {
		testCost, err := bcrypt.Cost([]byte(svc.dummyHashForCompare()))
		require.NoError(t, err, name)
		assert.Equal(t, bcrypt.MinCost, testCost, name)
	}
}

// TestLogin_OWASPEnumerationInvariant guards against regression that re-introduces
// distinct error messages for the 6 authentication failure modes (OWASP ASVS V3.3.4
// / V14.2). All 6 paths MUST return an identical string so the login endpoint cannot
// be used as an email-existence oracle.
//
// If this test fails after a code change, that change almost certainly re-introduced
// a username-enumeration vulnerability and MUST be reverted or fixed before landing.
//
// Issue #416 added the "store error" scenario: the real Postgres store returns
// (nil, pgx.ErrNoRows) for a missing row, not (nil, nil). Both the error path and
// the nil-user path now collapse to the same message "Check your email address and password and try again"
// and the Login function runs a dummy bcrypt compare to equalize response timing.
func TestLogin_OWASPEnumerationInvariant(t *testing.T) {
	const wantMsg = "Check your email address and password and try again"
	ctx := context.Background()

	lockUntil := time.Now().Add(10 * time.Minute)

	type scenario struct {
		storeError error
		getUser    func(t *testing.T) *User
		name       string
	}

	scenarios := []scenario{
		{
			name:    "user not found (nil, nil from store)",
			getUser: func(t *testing.T) *User { return nil },
		},
		{
			// Real Postgres store path: scanUser returns pgx.ErrNoRows for a
			// missing row, so GetUserByEmail returns (nil, error) not (nil, nil).
			name:       "user not found (nil, error from store)",
			getUser:    func(t *testing.T) *User { return nil },
			storeError: fmt.Errorf("no rows in result set"),
		},
		{
			name: "inactive account",
			getUser: func(t *testing.T) *User {
				u := createTestUser(t, "Correct1!")
				u.Active = false
				return u
			},
		},
		{
			name: "account locked (within lockout window)",
			getUser: func(t *testing.T) *User {
				u := createTestUser(t, "Correct1!")
				u.LockedUntil = &lockUntil
				return u
			},
		},
		{
			name: "empty password hash (no password set)",
			getUser: func(t *testing.T) *User {
				u := createTestUser(t, "Correct1!")
				u.PasswordHash = ""
				return u
			},
		},
		{
			name: "wrong password",
			getUser: func(t *testing.T) *User {
				return createTestUser(t, "Correct1!")
			},
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			mockStore := new(MockStore)
			mockEmail := new(MockEmailSender)
			service := createTestService(mockStore, mockEmail)

			user := sc.getUser(t)
			mockStore.On("GetUserByEmail", ctx, "test@example.com").Return(user, sc.storeError).Once()
			mockStore.On("RecordFailedLogin", ctx, mock.AnythingOfType("string")).Return(nil).Maybe()

			req := LoginRequest{
				Email:    "test@example.com",
				Password: "WrongPassword9!",
			}

			_, err := service.Login(ctx, req)
			require.Error(t, err, "expected login to fail for scenario %q", sc.name)
			assert.Equal(t, wantMsg, err.Error(),
				"scenario %q must return the uniform copy; distinct messages leak enumeration signal", sc.name)

			mockStore.AssertExpectations(t)
			mockEmail.AssertExpectations(t)
		})
	}
}
