package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/mail"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
)

// Configuration constants.
const (
	// PasswordResetExpiry is how long password reset tokens are valid.
	PasswordResetExpiry = 1 * time.Hour

	// PasswordSetupExpiry is how long an invited user has to set their
	// initial password via the link in the invite email. Longer than
	// PasswordResetExpiry because invites typically wait in an inbox
	// before the recipient acts on them.
	PasswordSetupExpiry = 7 * 24 * time.Hour

	// DefaultSessionDurationHours is the default session duration in hours.
	DefaultSessionDurationHours = 24

	// Account lockout settings for brute-force protection
	// MaxFailedLoginAttempts is the number of failed attempts before lockout.
	MaxFailedLoginAttempts = 5

	// AccountLockoutDuration is how long an account is locked after max failed attempts.
	AccountLockoutDuration = 15 * time.Minute

	// genericLoginError is returned for every authentication failure so callers
	// cannot distinguish a missing account from a wrong password (anti-enumeration,
	// closes issues #416 and #993).
	genericLoginError = "Check your email address and password and try again"
)

// Service handles authentication and authorization.
type Service struct {
	store       StoreInterface
	emailSender EmailSenderInterface
	// pendingUsage accumulates per-API-key request counts that have not yet
	// been flushed to the store, keyed by API key ID, with a flag marking the
	// key's single in-flight flusher. pendingUsageMu guards the map itself;
	// the entries are updated atomically without it. See
	// Service.RecordUsageAsync for why the count is buffered instead of
	// incremented once per flush.
	pendingUsage       map[string]*apiKeyUsageEntry
	pendingUsageMu     sync.Mutex
	onPasswordChange   func(ctx context.Context, userID, newPassword string)
	dashboardURL       string
	csrfKey            []byte
	sessionDuration    time.Duration
	bcryptCostOverride int
}

// ServiceConfig holds configuration for the auth service.
type ServiceConfig struct {
	Store            StoreInterface
	EmailSender      EmailSenderInterface
	OnPasswordChange func(ctx context.Context, userID, newPassword string)
	DashboardURL     string
	CSRFKey          []byte
	SessionDuration  time.Duration
	// BcryptCost lowers the bcrypt work factor for tests that build the
	// service through other packages. Zero, the only value production
	// uses, keeps bcryptCost (12) and the fixed cost-12 timing sentinel.
	BcryptCost int
}

// NewService creates a new auth service.
func NewService(cfg ServiceConfig) *Service {
	if cfg.SessionDuration == 0 {
		cfg.SessionDuration = time.Duration(DefaultSessionDurationHours) * time.Hour
	}

	// Empty DashboardURL produces broken setup/reset emails because the
	// service layer constructs links as fmt.Sprintf("%s/reset-password?...",
	// dashboardURL, token) — an empty prefix yields "/reset-password?token=..."
	// which is unclickable in any MUA. Surface this loudly at startup so
	// operators see the misconfiguration before the first user gets a broken
	// invite email rather than after. See issue #355.
	if cfg.DashboardURL == "" {
		logging.Warnf("auth: DashboardURL is empty — invite and password-reset emails will contain relative links unusable in mail clients. Set DASHBOARD_URL to the customer-facing dashboard origin.")
	}

	// SECURITY: Validate that dashboard URL uses HTTPS in production
	// This prevents password reset tokens from being leaked over HTTP
	if cfg.DashboardURL != "" && !strings.HasPrefix(cfg.DashboardURL, "https://") {
		// Allow http for localhost development only
		if !strings.HasPrefix(cfg.DashboardURL, "http://localhost") &&
			!strings.HasPrefix(cfg.DashboardURL, "http://127.0.0.1") {
			// Strip query string before logging to avoid leaking tokens in log output
			safeURL := cfg.DashboardURL
			if idx := strings.IndexByte(safeURL, '?'); idx >= 0 {
				safeURL = safeURL[:idx]
			}
			logging.Warnf("SECURITY WARNING: Dashboard URL does not use HTTPS: %s. Password reset links may be insecure.", safeURL)
		}
	}

	csrfKey := cfg.CSRFKey
	if len(csrfKey) == 0 {
		// No key supplied: generate a random ephemeral key. CSRF tokens issued
		// before a restart will become invalid (re-login required). Operators
		// should supply a stable key via CSRFKey to avoid this on restarts.
		logging.Warnf("auth: CSRFKey not set — generating ephemeral key; all CSRF tokens will be invalidated on service restart. Set CSRFKey to a stable 32-byte value.")
		csrfKey = make([]byte, 32)
		if _, err := rand.Read(csrfKey); err != nil {
			// rand.Read only fails on catastrophic OS errors; panic is appropriate.
			panic(fmt.Sprintf("auth: failed to generate ephemeral CSRF key: %v", err))
		}
	}

	return &Service{
		store:            cfg.Store,
		emailSender:      cfg.EmailSender,
		sessionDuration:  cfg.SessionDuration,
		dashboardURL:     cfg.DashboardURL,
		onPasswordChange: cfg.OnPasswordChange,
		csrfKey:          csrfKey,
		// Zero leaves the production cost in hashPassword and Login.
		bcryptCostOverride: cfg.BcryptCost,
	}
}

// notifyPasswordChange calls the password change callback if configured.
func (s *Service) notifyPasswordChange(ctx context.Context, userID, newPassword string) {
	if s.onPasswordChange != nil {
		s.onPasswordChange(ctx, userID, newPassword)
	}
}

// ensureStore returns an error if the auth store is not initialized.
func (s *Service) ensureStore() error {
	if s.store == nil {
		return fmt.Errorf("auth store not initialized")
	}
	return nil
}

// Login authenticates a user and creates a session.
func (s *Service) Login(ctx context.Context, req LoginRequest) (*LoginResponse, error) {
	if err := s.ensureStore(); err != nil {
		return nil, err
	}

	// Use only the address portion to prevent RFC 5322 display-name attacks
	// e.g. `"Attacker" <user@host>` must not be forwarded verbatim to the store
	parsed, err := mail.ParseAddress(req.Email)
	if err != nil {
		return nil, fmt.Errorf("invalid email format")
	}
	req.Email = parsed.Address

	if req.MFAToken != "" {
		return s.loginWithMFAToken(ctx, req)
	}

	user, err := s.getUserAndValidateStatus(ctx, req.Email)
	if err != nil {
		// Run a dummy bcrypt compare so the response time for a non-existent
		// user is indistinguishable from a wrong-password attempt on a real
		// account. Without this, the missing-row path returns immediately
		// (no bcrypt), leaking account existence via timing (issue #416).
		s.verifyPassword(req.Password, s.dummyHashForCompare())
		return nil, err
	}

	if err := s.verifyPasswordAndMFA(ctx, user, req); err != nil {
		if errors.Is(err, ErrMFARequired) {
			return nil, s.issueMFAChallenge(ctx, user)
		}
		return nil, err
	}

	return s.completeSuccessfulLogin(ctx, user)
}

// getUserAndValidateStatus retrieves user and checks if account is active and unlocked.
func (s *Service) getUserAndValidateStatus(ctx context.Context, email string) (*User, error) {
	user, err := s.store.GetUserByEmail(ctx, email)
	if err != nil || user == nil {
		// Return the same generic message for both "user not found" and store
		// errors so callers cannot distinguish a missing account from a DB
		// failure (issue #416). The caller (Login) runs a dummy bcrypt compare
		// after this to equalize response time with the wrong-password path.
		return nil, errors.New(genericLoginError) //nolint:staticcheck // ST1005: user-facing message; capitalization intentional and asserted by tests
	}

	if !user.Active {
		return nil, errors.New(genericLoginError) //nolint:staticcheck // ST1005: user-facing message; capitalization intentional and asserted by tests
	}

	if accountLocked(user) {
		remainingTime := time.Until(*user.LockedUntil).Round(time.Minute)
		// Omit user.ID from log to avoid leaking internal identifiers to log
		logging.Warnf("Login attempt for locked account (locked for %v more)", remainingTime)
		return nil, errors.New(genericLoginError) //nolint:staticcheck // ST1005: user-facing message; capitalization intentional and asserted by tests
	}
	// NOTE: when LockedUntil is set but the window has already expired, the user falls
	// through here with FailedLoginAttempts and LockedUntil still set in memory.
	// These fields are cleared in completeSuccessfulLogin after a successful password
	// check. If the password check fails, recordFailedLogin re-locks the account
	// correctly. This two-site invariant is intentional: clearing stale lock state
	// here would require an extra store write on every login attempt for a
	// previously-locked user, with no security benefit.

	return user, nil
}

// accountLocked reports whether the user's lockout window is still open.
func accountLocked(user *User) bool {
	return user.LockedUntil != nil && time.Now().Before(*user.LockedUntil)
}

// verifyPasswordAndMFA verifies password and MFA code if enabled.
//
// Returns ErrMFARequired (sentinel) when the user has MFA enabled but
// the request didn't carry a code, so the API handler can map to a
// machine-readable response (`{"error":"mfa_required"}`) rather than
// a generic 401. Returns ErrInvalidMFACode (sentinel) when the code
// was provided but didn't match TOTP or a persistently consumed recovery code.
//
// Accepts either a TOTP code OR a single-use recovery code as proof
// of MFA. Consumed recovery codes are removed from the user row on
// success — the success path persists the updated codes slice via
// ConsumeMFARecoveryCode before returning. A failed recovery-code attempt does
// NOT consume anything (the consumeRecoveryCode call only mutates
// the slice on a match).
func (s *Service) verifyPasswordAndMFA(ctx context.Context, user *User, req LoginRequest) error {
	// Use a generic error for missing password hash to avoid leaking account state
	// (a distinct message would reveal that the account exists but has no password set).
	// Both branches return the same message as the "user not found" path so the
	// full login failure surface is uniform (issue #416).
	if user.PasswordHash == "" {
		return errors.New(genericLoginError) //nolint:staticcheck // ST1005: user-facing message; capitalization intentional and asserted by tests
	}

	if !s.verifyPassword(req.Password, user.PasswordHash) {
		s.recordFailedLogin(ctx, user)
		return errors.New(genericLoginError) //nolint:staticcheck // ST1005: user-facing message; capitalization intentional and asserted by tests
	}

	if user.MFAEnabled {
		if req.MFACode == "" {
			return ErrMFARequired
		}
		return s.verifyLoginMFACode(ctx, user, req.MFACode)
	}

	return nil
}

// verifyLoginMFACode checks a TOTP or recovery code for an MFA-enabled user
// at login, for both the password + code form and the mfa_token form. A wrong
// code counts toward the account lockout.
func (s *Service) verifyLoginMFACode(ctx context.Context, user *User, code string) error {
	if user.MFASecret == "" {
		// Data integrity anomaly: MFA is flagged enabled but has no secret.
		// Log internally for operator visibility without leaking internal state
		// to the caller -- a distinct message would confirm the password was correct.
		logging.Errorf("MFA enabled but secret missing for user %s -- possible data integrity issue", user.ID)
		return errors.New(genericLoginError) //nolint:staticcheck // ST1005: user-facing message; capitalization intentional and asserted by tests
	}
	// verifyTOTP fails closed on empty or malformed inputs: empty code, empty
	// secret, and base32-decode errors all return false rather than a match.
	claimed, err := s.verifyAndClaimTOTP(ctx, user.ID, user.MFASecret, code)
	if err != nil {
		logging.Warnf("Failed to record TOTP use for user %s: %v", user.ID, err)
		return ErrInvalidMFACode
	}
	if claimed {
		return nil
	}
	// TOTP miss — try a recovery code. consumeRecoveryCode mutates
	// the user's slice on a match; persist the slice so the
	// consumed code can't be reused.
	readCodes := slices.Clone(user.MFARecoveryCodes)
	if s.consumeRecoveryCode(user, code) {
		if err := s.store.ConsumeMFARecoveryCode(ctx, user.ID, readCodes, user.MFARecoveryCodes); err != nil {
			logging.Warnf("Failed to persist recovery-code consumption for user %s: %v", user.ID, err)
			return ErrInvalidMFACode
		}
		return nil
	}
	s.recordFailedLogin(ctx, user)
	return ErrInvalidMFACode
}

// completeSuccessfulLogin creates session and updates user login info.
func (s *Service) completeSuccessfulLogin(ctx context.Context, user *User) (*LoginResponse, error) {
	session, err := s.createSession(ctx, user, "", "")
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}

	// The session already exists; bookkeeping failure must not hide its token.
	if err := s.store.RecordSuccessfulLogin(ctx, user.ID); err != nil {
		logging.Warnf("Failed to update login info for user %s: %v", user.ID, err)
	}

	return &LoginResponse{
		Token:     session.Token,
		ExpiresAt: session.ExpiresAt,
		User: &UserInfo{
			ID:         user.ID,
			Email:      user.Email,
			Groups:     user.GroupIDs,
			MFAEnabled: user.MFAEnabled,
		},
		CSRFToken: session.CSRFToken,
	}, nil
}

// Logout invalidates a session.
func (s *Service) Logout(ctx context.Context, token string) error {
	if err := s.ensureStore(); err != nil {
		return err
	}
	if token == "" {
		return fmt.Errorf("token is required")
	}
	hashedToken := hashSessionToken(token)
	return s.store.DeleteSession(ctx, hashedToken)
}

// ValidateSession checks if a session is valid and returns user info.
func (s *Service) ValidateSession(ctx context.Context, token string) (*Session, error) {
	if err := s.ensureStore(); err != nil {
		return nil, err
	}

	hashedToken := hashSessionToken(token)

	session, err := s.store.GetSession(ctx, hashedToken)
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, fmt.Errorf("session not found")
	}

	// Store-integrity check, not a timing defense (issue #313). GetSession
	// selected this row by `WHERE token = $1` on hashedToken, so the two are
	// equal unless the store returned a row for a different token; refuse it
	// if it ever does. The equality timing of that lookup is harmless: the
	// value compared is the SHA-256 of a 256-bit random token, which an
	// attacker can neither guess nor steer byte by byte.
	if subtle.ConstantTimeCompare([]byte(session.Token), []byte(hashedToken)) != 1 {
		return nil, fmt.Errorf("session not found")
	}

	if session.ExpiresAt.IsZero() {
		return nil, fmt.Errorf("session has no expiry: data integrity error")
	}

	if time.Now().After(session.ExpiresAt) {
		if delErr := s.store.DeleteSession(ctx, hashedToken); delErr != nil {
			logging.Warnf("Failed to delete expired session: %v", delErr)
		}
		return nil, fmt.Errorf("session expired")
	}

	if err := s.checkSessionUser(ctx, session); err != nil {
		return nil, err
	}

	// Return a copy of the session with the original token (not the hash) for client use.
	// Copying avoids mutating a potentially-shared store-cached pointer.
	result := *session
	result.Token = token
	return &result, nil
}

// checkSessionUser rejects a session whose user can no longer use it.
func (s *Service) checkSessionUser(ctx context.Context, session *Session) error {
	// UpdateUser revokes sessions on deactivation; this rejects any session that
	// outlives that (a failed delete, or Active flipped another way). Issue #89.
	user, err := s.loadUser(ctx, session.UserID)
	if err != nil {
		return fmt.Errorf("failed to load session user: %w", err)
	}
	if !user.Active {
		return fmt.Errorf("session not found")
	}
	// A password change deletes sessions only best-effort, and a login that
	// verified the old password can insert its session after that delete.
	// The version bump in the password write itself is what revokes them
	// (issue #398).
	if session.PasswordVersion != user.PasswordVersion {
		return ErrSessionPasswordRotated
	}
	return nil
}

// ValidateCSRFToken validates the CSRF token for a session.
//
// The expected CSRF token is derived as HMAC-SHA256(csrfKey, rawSessionToken),
// matching the token produced by createSession. Validation never reads the
// stored csrf_token column; it recomputes the MAC from the raw session token
// so a database read (SQLi, backup, replica) cannot yield a usable CSRF token.
func (s *Service) ValidateCSRFToken(ctx context.Context, sessionToken, csrfToken string) error {
	if csrfToken == "" {
		return fmt.Errorf("CSRF token is required")
	}

	// ValidateSession is called for its side-effects (expiry check, session
	// existence) and to obtain the raw session token for HMAC derivation.
	// The session object itself is not used for the CSRF comparison.
	if _, err := s.ValidateSession(ctx, sessionToken); err != nil {
		return fmt.Errorf("invalid session: %w", err)
	}

	// Derive the expected CSRF token from the raw session token and the
	// server-side key. This is identical to what createSession produced.
	expected := deriveCSRFToken(s.csrfKey, sessionToken)

	// Use constant-time comparison to prevent timing attacks.
	if subtle.ConstantTimeCompare([]byte(expected), []byte(csrfToken)) != 1 {
		return fmt.Errorf("invalid CSRF token")
	}

	return nil
}

// CleanupExpiredSessions removes expired sessions from the store.
func (s *Service) CleanupExpiredSessions(ctx context.Context) error {
	if err := s.ensureStore(); err != nil {
		return err
	}
	return s.store.CleanupExpiredSessions(ctx)
}

// Ping checks the health of the auth store database connection.
func (s *Service) Ping(ctx context.Context) error {
	if err := s.ensureStore(); err != nil {
		return err
	}
	return s.store.Ping(ctx)
}
