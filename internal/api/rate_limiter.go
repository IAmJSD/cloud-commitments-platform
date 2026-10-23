// Package api provides the HTTP API handlers for the CUDly dashboard.
package api

import (
	"context"
	"net/mail"
	"strings"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
)

// RateLimitConfig defines the rate limiting parameters for a specific endpoint/operation.
type RateLimitConfig struct {
	MaxAttempts int           // Maximum number of attempts allowed
	WindowSecs  int           // Time window in seconds
	Window      time.Duration // Computed time window (for convenience)
}

// NewRateLimitConfig creates a new RateLimitConfig.
func NewRateLimitConfig(maxAttempts, windowSecs int) RateLimitConfig {
	return RateLimitConfig{
		MaxAttempts: maxAttempts,
		WindowSecs:  windowSecs,
		Window:      time.Duration(windowSecs) * time.Second,
	}
}

// getDefaultRateLimits returns default rate limit configurations.
func getDefaultRateLimits() map[string]RateLimitConfig {
	return map[string]RateLimitConfig{
		"login":                 NewRateLimitConfig(5, 15*60),  // 5 attempts / 15 minutes / IP
		"forgot_password":       NewRateLimitConfig(10, 5*60),  // 10 attempts / 5 minutes / email
		"reset_password":        NewRateLimitConfig(10, 15*60), // 10 attempts / 15 minutes / IP
		"change_password":       NewRateLimitConfig(10, 15*60), // 10 attempts / 15 minutes / IP
		"api_general":           NewRateLimitConfig(300, 60),   // 300 requests / minute / IP
		"admin":                 NewRateLimitConfig(30, 60),    // 30 / minute / user
		"approve_cancel_public": NewRateLimitConfig(30, 60),    // 30 / minute / IP for public approve/cancel/reject
		// forgot_password_ip caps one client's total volume across addresses:
		// the per-email forgot_password bucket lets a caller walk many
		// addresses, one request each (#13). Same budget as reset_password,
		// the next step of the same flow.
		"forgot_password_ip": NewRateLimitConfig(10, 15*60), // 10 attempts / 15 minutes / IP (#13)
		// setup_admin should fire at most once per deployment. Issue #411 specifies
		// 5 attempts per 15 minutes so a cold-start burst cannot brute-force the
		// first-run endpoint before the DB-backed limiter is available.
		"setup_admin": NewRateLimitConfig(5, 15*60), // 5 attempts / 15 minutes / IP
		// register is a public, side-effect-heavy endpoint (DB write + email fan-out
		// per request). Issue #1016: without a dedicated bucket it fell through to
		// api_general (300/min), enabling bulk table-flooding and inbox-spam. Limit
		// to 5 per 15 minutes per IP, matching setup_admin severity.
		"register": NewRateLimitConfig(5, 15*60), // 5 attempts / 15 minutes / IP (#1016)
		// mfa covers every /api/auth/mfa/* route that checks a password, TOTP or
		// recovery code. Applied per IP AND per session user (#94), so rotating
		// source IPs does not reset one account's guessing budget.
		"mfa": NewRateLimitConfig(10, 15*60), // 10 attempts / 15 minutes / IP and / user (#94)
		// login_mfa is Login's TOTP / recovery-code leg, keyed per account email
		// on top of the per-IP login bucket, so rotating source IPs does not
		// reset one account's code-guessing budget (#442).
		"login_mfa": NewRateLimitConfig(10, 15*60), // 10 attempts / 15 minutes / email (#442)
	}
}

// assertRateLimitKeysKnown panics at startup if any endpoint key passed to
// checkRateLimit is absent from the default rate-limit map. A missing key
// silently falls through to api_general (300/min), which is the root cause of
// issue #1016. Call this once during handler initialisation, passing every
// string literal that appears in checkRateLimit call sites.
func assertRateLimitKeysKnown(keys ...string) {
	limits := getDefaultRateLimits()
	for _, k := range keys {
		if _, ok := limits[k]; !ok {
			panic("checkRateLimit called with unknown endpoint key " + k +
				": add it to getDefaultRateLimits() (issue #1016)")
		}
	}
}

// checkLoginMFARateLimit throttles Login's TOTP / recovery-code leg per account
// (#442): the "login" bucket is per IP only, so a caller holding the password
// could rotate source IPs to keep guessing codes. It runs only when the request
// carries an MFA code, before any password or code is checked, and fails closed
// like checkRateLimitStrict.
//
// The key is the email exactly as Login resolves it (display name stripped by
// mail.ParseAddress), lower-cased so case variants share one budget. An email
// Login would reject as malformed is not counted; Login fails before any code
// is checked. A caller without the password can spend a victim's budget, but
// wrong passwords already lock that account for AccountLockoutDuration, so this
// adds no new denial of service. The 429 does not depend on whether the
// account exists.
func (h *Handler) checkLoginMFARateLimit(ctx context.Context, email, mfaCode string) error {
	if h.rateLimiter == nil || mfaCode == "" {
		return nil
	}
	parsed, err := mail.ParseAddress(email)
	if err != nil {
		return nil
	}
	const endpoint = "login_mfa"
	key := strings.ToLower(parsed.Address)
	allowed, err := h.rateLimiter.AllowWithEmail(ctx, key, endpoint)
	if err != nil {
		logging.Errorf("ALERT: rate limiter error on credential endpoint %s for %s; request denied (02-M1): %v",
			endpoint, redactEmail(key), err)
		return NewClientError(503, "service temporarily unavailable, please try again later")
	}
	if !allowed {
		logging.Warnf("Rate limit exceeded for %s for: %s", endpoint, redactEmail(key))
		return NewClientError(429, "too many requests, please try again later")
	}
	return nil
}
