package api

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func loginReqFrom(ip, email, code string) *events.LambdaFunctionURLRequest {
	body := fmt.Sprintf(`{"email":%q,"password":%q,"mfa_code":%q}`, email, b64("pw"), code)
	if code == "" {
		body = fmt.Sprintf(`{"email":%q,"password":%q}`, email, b64("pw"))
	}
	req := &events.LambdaFunctionURLRequest{Body: body}
	req.RequestContext.HTTP.SourceIP = ip
	return req
}

func requireClientCode(t *testing.T, err error, code int, msgAndArgs ...any) {
	t.Helper()
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a client error, got %v", err)
	assert.Equal(t, code, ce.code, msgAndArgs...)
}

// Issue #442: Login's TOTP / recovery-code leg is throttled per account, so
// rotating source IPs does not reset the code-guessing budget. Email variants
// that Login resolves to the same address share one budget.
func TestHandler_login_MFALegRateLimitedPerEmail(t *testing.T) {
	limit := getDefaultRateLimits()["login_mfa"].MaxAttempts
	require.Less(t, limit, 100)
	emails := []string{"victim@example.com", "Victim@Example.com", `"Someone" <victim@example.com>`}

	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("Login", mock.Anything, mock.Anything).
		Return((*LoginResponse)(nil), ErrInvalidMFACode_test()).Times(limit)
	h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}

	for i := 0; i <= limit; i++ {
		ip := fmt.Sprintf("198.51.100.%d", i+1) // fresh IP each time: the per-IP bucket never trips
		_, err := h.login(context.Background(), loginReqFrom(ip, emails[i%len(emails)], "000000"))
		if i < limit {
			requireClientCode(t, err, 401, "attempt %d", i+1)
		} else {
			requireClientCode(t, err, 429, "attempt %d must be throttled", i+1)
		}
	}

	// Another account keeps its own budget.
	mockAuth.On("Login", mock.Anything, mock.Anything).
		Return((*LoginResponse)(nil), ErrInvalidMFACode_test()).Once()
	_, err := h.login(context.Background(), loginReqFrom("203.0.113.1", "other@example.com", "000000"))
	requireClientCode(t, err, 401)
}

// The password-only first leg never touches the per-email bucket, so users
// without MFA and the mfa_required round trip are unaffected.
func TestHandler_login_NoMFACodeSkipsPerEmailBucket(t *testing.T) {
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("Login", mock.Anything, mock.Anything).
		Return((*LoginResponse)(nil), ErrMFARequired_test()).Once()
	rl := new(MockRateLimiter)
	t.Cleanup(func() { rl.AssertExpectations(t) })
	rl.On("AllowWithIP", mock.Anything, "198.51.100.1", "login").Return(true, nil).Once()
	h := &Handler{auth: mockAuth, rateLimiter: rl}

	_, err := h.login(context.Background(), loginReqFrom("198.51.100.1", "u@example.com", ""))
	requireClientCode(t, err, 401)
	rl.AssertNotCalled(t, "AllowWithEmail", mock.Anything, mock.Anything, mock.Anything)
}

func TestHandler_login_MFALegRateLimiterErrorFailsClosed(t *testing.T) {
	mockAuth := new(MockAuthService)
	rl := new(MockRateLimiter)
	t.Cleanup(func() { rl.AssertExpectations(t) })
	rl.On("AllowWithIP", mock.Anything, mock.Anything, "login").Return(true, nil).Once()
	rl.On("AllowWithEmail", mock.Anything, "u@example.com", "login_mfa").Return(false, errors.New("db down")).Once()
	h := &Handler{auth: mockAuth, rateLimiter: rl}

	_, err := h.login(context.Background(), loginReqFrom("198.51.100.1", "U@example.com", "000000"))
	requireClientCode(t, err, 503)
	mockAuth.AssertNotCalled(t, "Login", mock.Anything, mock.Anything)
}

// Issue #442: requireMFASession checks the session before the per-IP "mfa"
// bucket, so requests without a valid session cannot spend the budget of
// users behind the same IP.
func TestHandler_mfaEndpoints_InvalidSessionDoesNotSpendIPBudget(t *testing.T) {
	limit := getDefaultRateLimits()["mfa"].MaxAttempts
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("ValidateSession", mock.Anything, "bad").Return((*Session)(nil), errors.New("invalid")).Times(limit + 1)
	mockAuth.On("ValidateSession", mock.Anything, "tok").Return(&Session{UserID: "user-1"}, nil).Once()
	mockAuth.On("MFAEnableAPI", mock.Anything, "user-1", "000000").Return([]string(nil), nil).Once()
	h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}

	for i := 0; i <= limit; i++ {
		req := authedReq("bad", `{"code":"000000"}`)
		req.RequestContext.HTTP.SourceIP = "198.51.100.1"
		_, err := h.mfaEnable(context.Background(), req)
		requireClientCode(t, err, 401, "attempt %d", i+1)
	}

	req := authedReq("tok", `{"code":"000000"}`)
	req.RequestContext.HTTP.SourceIP = "198.51.100.1"
	_, err := h.mfaEnable(context.Background(), req)
	require.NoError(t, err, "a valid session on the same IP must still have its budget")
}
