package api

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const forgotPasswordOK = "if the email exists, a reset link has been sent"

func forgotReqFrom(ip, body string) *events.LambdaFunctionURLRequest {
	req := &events.LambdaFunctionURLRequest{Body: body}
	req.RequestContext.HTTP.SourceIP = ip
	return req
}

func forgotBody(email string) string {
	return fmt.Sprintf(`{"email":%q}`, email)
}

func requireForgotOK(t *testing.T, result any, err error, msgAndArgs ...any) {
	t.Helper()
	require.NoError(t, err, msgAndArgs...)
	resp, ok := result.(map[string]string)
	require.True(t, ok, "unexpected result type %T", result)
	assert.Equal(t, forgotPasswordOK, resp["status"], msgAndArgs...)
}

// The per-IP and per-email limits on forgot-password are separate buckets.
// Pinning both keeps the handler comment and the config in step (#13).
func TestGetDefaultRateLimits_ForgotPasswordBuckets(t *testing.T) {
	limits := getDefaultRateLimits()
	assert.Equal(t, NewRateLimitConfig(10, 5*60), limits["forgot_password"], "per-email bucket")
	assert.Equal(t, NewRateLimitConfig(10, 15*60), limits["forgot_password_ip"], "per-IP bucket")
	assert.NotPanics(t, func() { assertRateLimitKeysKnown("forgot_password", "forgot_password_ip") })
}

// Issue #13: one client walking many addresses (one request per address) used
// to be allowed without limit because only the per-email bucket existed.
func TestHandler_forgotPassword_PerIPCapAcrossDistinctEmails(t *testing.T) {
	limit := getDefaultRateLimits()["forgot_password_ip"].MaxAttempts
	require.Less(t, limit, 100)

	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("RequestPasswordReset", mock.Anything, mock.Anything).Return(nil)
	h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}
	ctx := context.Background()

	for i := 0; i <= limit; i++ {
		result, err := h.forgotPassword(ctx, forgotReqFrom("203.0.113.7", forgotBody(fmt.Sprintf("probe%d@example.com", i))))
		if i < limit {
			requireForgotOK(t, result, err, "attempt %d", i+1)
			continue
		}
		assert.Nil(t, result)
		requireClientCode(t, err, 429, "attempt %d must be throttled", i+1)
	}
	mockAuth.AssertNumberOfCalls(t, "RequestPasswordReset", limit)

	// A different client keeps its own budget.
	result, err := h.forgotPassword(ctx, forgotReqFrom("203.0.113.8", forgotBody("someone@example.com")))
	requireForgotOK(t, result, err)
	mockAuth.AssertNumberOfCalls(t, "RequestPasswordReset", limit+1)
}

// The bucket key is the request's SourceIP, which the HTTP server derives from
// the rightmost (proxy-appended) X-Forwarded-For entry. A forwarded-for header
// that reaches the handler must not open a fresh bucket.
func TestHandler_forgotPassword_PerIPCapIgnoresForwardedHeaders(t *testing.T) {
	limit := getDefaultRateLimits()["forgot_password_ip"].MaxAttempts

	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("RequestPasswordReset", mock.Anything, mock.Anything).Return(nil)
	h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}

	for i := 0; i <= limit; i++ {
		req := forgotReqFrom("203.0.113.9", forgotBody(fmt.Sprintf("spoof%d@example.com", i)))
		req.Headers = map[string]string{
			"x-forwarded-for": fmt.Sprintf("198.51.100.%d", i+1),
			"x-real-ip":       fmt.Sprintf("198.51.100.%d", i+1),
		}
		result, err := h.forgotPassword(context.Background(), req)
		if i < limit {
			requireForgotOK(t, result, err, "attempt %d", i+1)
		} else {
			requireClientCode(t, err, 429, "attempt %d must be throttled", i+1)
		}
	}
}

// The per-IP check runs before the body is parsed, so a flood of malformed
// bodies spends the same budget as well-formed ones.
func TestHandler_forgotPassword_MalformedBodiesSpendIPBudget(t *testing.T) {
	limit := getDefaultRateLimits()["forgot_password_ip"].MaxAttempts

	mockAuth := new(MockAuthService)
	h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}

	for i := 0; i < limit; i++ {
		_, err := h.forgotPassword(context.Background(), forgotReqFrom("203.0.113.10", "{not json"))
		requireClientCode(t, err, 400, "attempt %d", i+1)
	}
	_, err := h.forgotPassword(context.Background(), forgotReqFrom("203.0.113.10", forgotBody("user@example.com")))
	requireClientCode(t, err, 429)
	mockAuth.AssertNotCalled(t, "RequestPasswordReset", mock.Anything, mock.Anything)
}

// A request refused by the per-IP bucket must not spend the target address's
// per-email budget or reach the reset service.
func TestHandler_forgotPassword_PerIPDeniedSkipsEmailBucket(t *testing.T) {
	mockAuth := new(MockAuthService)
	rl := new(MockRateLimiter)
	t.Cleanup(func() { rl.AssertExpectations(t) })
	rl.On("AllowWithIP", mock.Anything, "203.0.113.11", "forgot_password_ip").Return(false, nil).Once()
	h := &Handler{auth: mockAuth, rateLimiter: rl}

	result, err := h.forgotPassword(context.Background(), forgotReqFrom("203.0.113.11", forgotBody("victim@example.com")))
	assert.Nil(t, result)
	requireClientCode(t, err, 429)
	rl.AssertNotCalled(t, "AllowWithEmail", mock.Anything, mock.Anything, mock.Anything)
	mockAuth.AssertNotCalled(t, "RequestPasswordReset", mock.Anything, mock.Anything)
}

// Like the per-email bucket (02-M1), the per-IP bucket fails open on a limiter
// error so the endpoint's response does not expose limiter health.
func TestHandler_forgotPassword_PerIPLimiterErrorFailsOpen(t *testing.T) {
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("RequestPasswordReset", mock.Anything, "user@example.com").Return(nil).Once()
	rl := new(MockRateLimiter)
	t.Cleanup(func() { rl.AssertExpectations(t) })
	rl.On("AllowWithIP", mock.Anything, "203.0.113.12", "forgot_password_ip").
		Return(false, errors.New("pool exhausted")).Once()
	rl.On("AllowWithEmail", mock.Anything, "user@example.com", "forgot_password").Return(true, nil).Once()
	h := &Handler{auth: mockAuth, rateLimiter: rl}

	result, err := h.forgotPassword(context.Background(), forgotReqFrom("203.0.113.12", forgotBody("user@example.com")))
	requireForgotOK(t, result, err)
}

// Concurrent requests from one client admit exactly the bucket size.
func TestHandler_forgotPassword_PerIPCapConcurrent(t *testing.T) {
	limit := getDefaultRateLimits()["forgot_password_ip"].MaxAttempts
	const callers = 40

	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("RequestPasswordReset", mock.Anything, mock.Anything).Return(nil)
	h := &Handler{auth: mockAuth, rateLimiter: NewInMemoryRateLimiter()}

	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.forgotPassword(context.Background(), forgotReqFrom("203.0.113.13", forgotBody(fmt.Sprintf("c%d@example.com", i))))
			code := 200
			if ce, ok := IsClientError(err); ok {
				code = ce.code
			} else if err != nil {
				code = -1
			}
			mu.Lock()
			codes[code]++
			mu.Unlock()
		}()
	}
	wg.Wait()

	assert.Equal(t, map[int]int{200: limit, 429: callers - limit}, codes)
	mockAuth.AssertNumberOfCalls(t, "RequestPasswordReset", limit)
}
