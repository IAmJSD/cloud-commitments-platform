package api

import (
	"context"
	"crypto/subtle"
	"fmt"
	"strings"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/aws/aws-lambda-go/events"
)

// isPublicEndpoint returns true for endpoints that don't require authentication.
func (h *Handler) isPublicEndpoint(path string) bool {
	// Prefix-matched public endpoints: dynamic routes where the path carries
	// a resource ID (approval/cancel/revoke tokens, register tokens, docs
	// sub-paths) and static prefixes that are always fully public.
	publicPrefixEndpoints := []string{
		"/health",     // Root health endpoint (no /api prefix)
		"/api/health", // API health endpoint
		"/api/purchases/approve/",
		"/api/purchases/cancel/",
		"/api/purchases/revoke/",
		"/api/ri-exchange/approve/",
		"/api/ri-exchange/reject/",
		"/api/auth/login",
		"/api/auth/check-admin",
		"/api/auth/setup-admin",
		"/api/auth/forgot-password",
		"/api/auth/reset-password",
		"/api/register/", // GET /api/register/:token (trailing slash avoids matching /api/registrations)
		"/api/notifications/unsubscribe",
		"/docs",
		"/api/docs",
	}
	for _, ep := range publicPrefixEndpoints {
		if strings.HasPrefix(path, ep) {
			return true
		}
	}
	// Exact-match singleton public endpoints. These must not be prefix-matched
	// to prevent "/version-anything" or "/api/register-anything" from
	// bypassing auth via accidental prefix overlap.
	switch path {
	case "/version", // Public build-version endpoint (version / git SHA / build time)
		"/api/info",     // Not /api/info/deployment, which is AuthUser
		"/api/register": // POST /api/register (no trailing slash)
		return true
	}
	return false
}

// PrincipalKind identifies which credential type was used to authenticate.
type PrincipalKind string

const (
	// PrincipalAdminAPIKey is set when the request authenticated with the
	// shared admin API key (X-API-Key header matching h.apiKey).
	PrincipalAdminAPIKey PrincipalKind = "admin-api-key"
	// PrincipalUserAPIKey is set when the request authenticated with a
	// per-user API key issued via /api/api-keys.
	PrincipalUserAPIKey PrincipalKind = "user-api-key" // #nosec G101 -- credential-type label, not a credential value
	// PrincipalSession is set when the request authenticated with a
	// bearer-token session (X-Authorization / Authorization header).
	PrincipalSession PrincipalKind = "session"
)

// Principal carries the resolved caller identity returned by
// authenticatePrincipal and requireAuth. Handlers that need the caller's
// identity read it from here rather than re-resolving it through a second
// ValidateSession / ValidateUserAPIKeyAPI call.
type Principal struct {
	// Field order groups the pointer-bearing Session ahead of the trailing
	// string so the struct satisfies govet fieldalignment.
	Kind    PrincipalKind
	Session *Session // non-nil when a valid bearer session was presented
	UserID  string   // empty for PrincipalAdminAPIKey
	Email   string   // empty for PrincipalAdminAPIKey; populated for session/user-api-key
	// APIKeyID is the database ID of the user API key the request
	// authenticated with. Non-empty exactly when Kind is
	// PrincipalUserAPIKey; empty for every other kind. It lets
	// validateSecurityContext book the key's usage once per request without
	// re-validating the credential.
	APIKeyID string
}

type principalContextKey struct{}

func contextWithPrincipal(ctx context.Context, principal *Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

func principalFromContext(ctx context.Context) (*Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(*Principal)
	return principal, ok && principal != nil
}

// authenticatePrincipal checks the admin API key, user API key and Bearer
// token credentials and returns the fully resolved Principal. Returns a
// non-nil Principal on success; returns nil and a 401 ClientError when no
// valid credential is present.
func (h *Handler) authenticatePrincipal(ctx context.Context, req *events.LambdaFunctionURLRequest) (*Principal, error) {
	apiKey := extractAPIKey(req)

	if h.checkAdminAPIKey(apiKey) {
		return &Principal{Kind: PrincipalAdminAPIKey}, nil
	}

	if h.auth == nil {
		return nil, NewClientError(401, "authentication required")
	}

	userPrincipal := h.principalFromUserAPIKey(ctx, apiKey)
	sessionPrincipal := h.principalFromBearerToken(ctx, req)
	if userPrincipal != nil {
		if sessionPrincipal != nil {
			userPrincipal.Session = sessionPrincipal.Session
		}
		return userPrincipal, nil
	}
	if sessionPrincipal != nil {
		return sessionPrincipal, nil
	}

	return nil, NewClientError(401, "authentication required")
}

// principalFromUserAPIKey resolves a Principal from a user API key.
// Returns nil when the key is empty, validation fails, or the key/user
// records cannot be retrieved.
func (h *Handler) principalFromUserAPIKey(ctx context.Context, apiKey string) *Principal {
	if apiKey == "" {
		return nil
	}
	keyRaw, userRaw, err := h.auth.ValidateUserAPIKeyAPI(ctx, apiKey)
	if err != nil {
		logging.Debugf("User API key validation failed: %v", err)
		return nil
	}
	if userRaw == nil {
		return nil
	}
	// ValidateUserAPIKeyAPI returns *auth.User as any via the AuthServiceInterface.
	// Assert to the concrete type; if validation succeeded but the concrete type is
	// unexpected we deny rather than returning a partially-populated Principal: fail closed.
	user, ok := userRaw.(*auth.User)
	if !ok {
		logging.Debugf("User API key: userRaw has unexpected type (%T); denying", userRaw)
		return nil
	}
	// The key ID is required, not optional: it is what validateSecurityContext
	// books usage against, so accepting a principal without one would silently
	// stop counting that key's requests. A successful validation always yields
	// the key row, so a missing or mistyped one is a broken invariant -- deny
	// rather than authenticate a key we cannot attribute.
	key, ok := keyRaw.(*auth.UserAPIKey)
	if !ok || key == nil || key.ID == "" {
		logging.Debugf("User API key: validation returned no usable key record (%T); denying", keyRaw)
		return nil
	}
	return &Principal{
		Kind:     PrincipalUserAPIKey,
		UserID:   user.ID,
		Email:    user.Email,
		APIKeyID: key.ID,
	}
}

// principalFromBearerToken resolves a Principal from a session bearer token.
// Returns nil when no token is present or the session is invalid.
func (h *Handler) principalFromBearerToken(ctx context.Context, req *events.LambdaFunctionURLRequest) *Principal {
	token := h.extractBearerToken(req)
	if token == "" {
		return nil
	}
	session, err := h.auth.ValidateSession(ctx, token)
	if err != nil || session == nil {
		return nil
	}
	return &Principal{
		Kind:    PrincipalSession,
		UserID:  session.UserID,
		Email:   session.Email,
		Session: session,
	}
}

func extractAPIKey(req *events.LambdaFunctionURLRequest) string {
	apiKey := req.Headers["x-api-key"]
	if apiKey == "" {
		apiKey = req.Headers["X-API-Key"]
	}
	return apiKey
}

func (h *Handler) checkAdminAPIKey(apiKey string) bool {
	if apiKey != "" && h.apiKey != "" && subtle.ConstantTimeCompare([]byte(apiKey), []byte(h.apiKey)) == 1 {
		return true
	}
	return false
}

// extractBearerToken extracts the token from the Authorization or X-Authorization header
// Note: X-Authorization is used by the frontend because CloudFront OAC signs requests
// with SigV4, which overwrites the standard Authorization header.
func (h *Handler) extractBearerToken(req *events.LambdaFunctionURLRequest) string {
	// First check X-Authorization (used by frontend with CloudFront OAC)
	authHdr := req.Headers["x-authorization"]
	if authHdr == "" {
		authHdr = req.Headers["X-Authorization"]
	}
	// Fall back to standard Authorization header (for direct API access)
	if authHdr == "" {
		authHdr = req.Headers["authorization"]
	}
	if authHdr == "" {
		authHdr = req.Headers["Authorization"]
	}

	if strings.HasPrefix(strings.ToLower(authHdr), "bearer ") {
		return authHdr[len("bearer "):]
	}

	return ""
}

// requiresCSRFValidation returns true for state-changing requests that need CSRF protection.
//
// It is only consulted for non-public paths: validateSecurityContext returns
// before calling it whenever isPublicEndpoint(path) is true. AuthPublic routes
// that read a session therefore enforce CSRF inside their own handlers, and
// TestAuthPublicMoneyRoutes_SessionWithoutCSRFIsRefused checks that they do.
func (h *Handler) requiresCSRFValidation(method, path string) bool {
	// Every method except the safe ones (RFC 9110 section 9.2.1) can change
	// state, so it needs CSRF protection. Listing the safe methods rather than
	// the unsafe ones keeps PATCH, and any verb a route adds later, covered
	// (issue #1101).
	switch method {
	case "GET", "HEAD", "OPTIONS":
		return false
	}

	// Auth endpoints that don't have a session yet are exempt.
	//
	// Note: /api/purchases/approve/ and /api/purchases/cancel/ are NOT listed
	// here. Those routes are AuthPublic and listed in isPublicEndpoint(), so
	// validateSecurityContext() short-circuits before requiresCSRFValidation() is
	// reached on the token-only (email-link) path. For the session-authed path
	// (approvePurchaseViaSession / cancelPurchaseViaSession), CSRF is enforced
	// directly inside those functions -- a blanket middleware exemption would
	// leave session-authenticated POSTs to these endpoints unprotected (CSRF).
	//
	// Similarly, /api/ri-exchange/approve/ and /api/ri-exchange/reject/ are
	// AuthPublic and therefore exempted by isPublicEndpoint(), not here.
	// approve/ has a session-authed sub-path (approveRIExchangeViaSession)
	// and, like the purchases functions above, enforces CSRF directly
	// inside that function -- NOT here, and not automatically just because
	// it is "similar" to purchases (issue #1757: this exemption used to
	// exist in prose only, with no actual validateCSRF call backing it).
	// reject/ has no session-authed path at all -- it is unconditionally
	// gated on a constant-time comparison against the record's stored
	// approval token, so CSRF does not apply to it.
	//
	// All exemptions use exact-match to prevent a path like
	// /api/register-malicious from bypassing CSRF via accidental prefix overlap.
	// This mirrors the exact-match approach in isPublicEndpoint().
	switch path {
	case "/api/auth/login",
		"/api/auth/setup-admin",
		"/api/auth/forgot-password",
		"/api/auth/reset-password",
		"/api/register": // POST /api/register (public registration, no session)
		return false
	}

	return true
}

// validateCSRF validates the CSRF token from the request header.
func (h *Handler) validateCSRF(ctx context.Context, req *events.LambdaFunctionURLRequest) error {
	if h.auth == nil {
		return fmt.Errorf("authentication service not configured")
	}

	// API-key-authenticated requests bypass CSRF (no cookie-based session).
	if h.apiKeyBypassCSRF(ctx, req) {
		return nil
	}

	sessionToken := h.extractBearerToken(req)
	if sessionToken == "" {
		return fmt.Errorf("no session token for CSRF validation")
	}

	csrfToken := extractCSRFToken(req)
	logMissingCSRFToken(req, csrfToken)
	return h.auth.ValidateCSRFToken(ctx, sessionToken, csrfToken)
}

// apiKeyBypassCSRF returns true when the request presents a valid admin or
// user API key. API-key auth is stateless and not cookie-based, so CSRF
// protection doesn't apply. An invalid API key returns false — the caller
// then falls through to session-based CSRF validation.
func (h *Handler) apiKeyBypassCSRF(ctx context.Context, req *events.LambdaFunctionURLRequest) bool {
	if principal, ok := principalFromContext(ctx); ok {
		return principal.Kind == PrincipalAdminAPIKey || principal.Kind == PrincipalUserAPIKey
	}
	apiKey := req.Headers["x-api-key"]
	if apiKey == "" {
		apiKey = req.Headers["X-API-Key"]
	}
	if apiKey == "" {
		return false
	}
	if h.apiKey != "" && subtle.ConstantTimeCompare([]byte(apiKey), []byte(h.apiKey)) == 1 {
		return true
	}
	if _, _, err := h.auth.ValidateUserAPIKeyAPI(ctx, apiKey); err == nil {
		return true
	}
	return false
}

// extractCSRFToken pulls the CSRF token from the case-insensitive header.
func extractCSRFToken(req *events.LambdaFunctionURLRequest) string {
	if t := req.Headers["x-csrf-token"]; t != "" {
		return t
	}
	return req.Headers["X-CSRF-Token"]
}

// logMissingCSRFToken emits a defensive warn-level log when a session-
// authenticated request reaches CSRF validation with no token header.
// Almost always indicates a frontend regression (sessionStorage cleared, or
// a new fetch site forgot to include the token). ValidateCSRFToken still
// rejects the request -- this just makes the cause obvious in logs.
func logMissingCSRFToken(req *events.LambdaFunctionURLRequest, csrfToken string) {
	if csrfToken != "" {
		return
	}
	logging.Warnf("CSRF validation: empty header on session-authenticated %s %s from %s -- frontend regression?",
		req.RequestContext.HTTP.Method, redactURL(req.RequestContext.HTTP.Path), req.RequestContext.HTTP.SourceIP)
}

// redactURL replaces any token= query parameter value with REDACTED so
// approval/revocation tokens are never written to structured access logs.
// Applied to every URL or path string before it is passed to a logging call.
//
// Matches:
//
//	?token=<value>   at the start of the query string
//	&token=<value>   as a subsequent parameter
//
// The replacement is done with a simple string scan so there is no regexp
// compilation overhead on the hot request path.
func redactURL(u string) string {
	return redactQueryParam(u, "token")
}

// redactQueryParam replaces the value of the named query parameter in u with
// REDACTED. The match is case-sensitive and handles both ? and & prefixes.
func redactQueryParam(u, param string) string {
	// Handle both ?param=val and &param=val forms.
	for _, prefix := range []string{"?" + param + "=", "&" + param + "="} {
		idx := strings.Index(u, prefix)
		if idx == -1 {
			continue
		}
		start := idx + len(prefix)
		// Find the end of the value (next & or end of string).
		end := strings.IndexByte(u[start:], '&')
		if end == -1 {
			u = u[:start] + "REDACTED"
		} else {
			u = u[:start] + "REDACTED" + u[start+end:]
		}
	}
	return u
}

// requireAuth verifies the request carries a valid authentication credential
// of any kind (admin API key, user API key, or session bearer token).
//
// Used as a defense-in-depth check by Router.Route for AuthUser routes:
// HandleRequest normally places authenticatePrincipal's result in the request
// context before dispatch. Router.Route still calls this helper so standalone
// router use remains protected; an existing context principal is reused and a
// missing one is resolved here. Returns a 401 ClientError on failure.
func (h *Handler) requireAuth(ctx context.Context, req *events.LambdaFunctionURLRequest) (*Principal, error) {
	if principal, ok := principalFromContext(ctx); ok {
		return principal, nil
	}
	return h.authenticatePrincipal(ctx, req)
}

func (h *Handler) requireSessionPrincipal(ctx context.Context, req *events.LambdaFunctionURLRequest) (*Session, error) {
	if principal, ok := principalFromContext(ctx); ok {
		if principal.Session == nil {
			return nil, NewClientError(401, "session authentication required")
		}
		return principal.Session, nil
	}
	if h.auth == nil {
		return nil, fmt.Errorf("authentication service not configured")
	}
	token := h.extractBearerToken(req)
	if token == "" {
		return nil, NewClientError(401, "no authorization token provided")
	}
	session, err := h.auth.ValidateSession(ctx, token)
	if err != nil || session == nil {
		return nil, NewClientError(401, "invalid session")
	}
	return session, nil
}

// requireAdmin gates the coarse admin-only routes (AuthAdmin). "Admin" is now
// defined as holding the full-access {admin, *} capability, i.e. membership in
// the Administrators group. Accepts both the stateless admin API key (which
// bypasses the per-user lookup) and a Bearer-token session whose group-derived
// permissions include {admin, *}. Fail closed: a missing auth service, an
// invalid session, or a permission-lookup error denies access.
func (h *Handler) requireAdmin(ctx context.Context, req *events.LambdaFunctionURLRequest) (*Session, error) {
	var session *Session
	if principal, ok := principalFromContext(ctx); ok {
		if principal.Kind == PrincipalAdminAPIKey {
			return &Session{UserID: apiKeyAdminUserID}, nil
		}
		if principal.Session == nil {
			return nil, NewClientError(401, "session authentication required")
		}
		session = principal.Session
	} else {
		// Check admin API key first (stateless auth).
		apiKey := extractAPIKey(req)
		if h.checkAdminAPIKey(apiKey) {
			return &Session{UserID: apiKeyAdminUserID}, nil
		}
		if h.auth == nil {
			return nil, fmt.Errorf("authentication service not configured")
		}
		var err error
		session, err = h.requireSessionPrincipal(ctx, req)
		if err != nil {
			return nil, err
		}
	}

	if h.auth == nil {
		return nil, fmt.Errorf("authentication service not configured")
	}

	// HasPermissionAPI(admin, *) returns true only for users who hold the
	// full-access capability, i.e. Administrators-group members. Any other
	// user (including zero-group users) is denied.
	isAdmin, err := h.auth.HasPermissionAPI(ctx, session.UserID, auth.ActionAdmin, auth.ResourceAll)
	if err != nil {
		return nil, fmt.Errorf("admin permission check failed: %w", err)
	}
	if !isAdmin {
		return nil, NewClientError(403, "admin access required")
	}

	return session, nil
}

// checkRateLimit checks if the request is allowed based on IP-based rate limiting.
// Returns nil if allowed, or an error if rate limited.
//
// This function uses a fail-open design: if the rate limiter encounters an error
// (e.g. DB pool exhaustion, transient unavailability), the request is allowed
// through. Use checkRateLimitStrict for endpoints where availability of the
// rate limiter is itself a security requirement (credential brute-force surface).
func (h *Handler) checkRateLimit(ctx context.Context, req *events.LambdaFunctionURLRequest, endpoint string) error {
	if h.rateLimiter == nil {
		return nil
	}

	clientIP := req.RequestContext.HTTP.SourceIP
	allowed, err := h.rateLimiter.AllowWithIP(ctx, clientIP, endpoint)
	if err != nil {
		logging.Warnf("Rate limiter error for IP %s: %v", clientIP, err)
		// Continue on rate limiter errors to avoid blocking legitimate requests
		return nil
	}
	if !allowed {
		logging.Warnf("Rate limit exceeded for %s from IP: %s", endpoint, clientIP)
		return NewClientError(429, "too many requests, please try again later")
	}
	return nil
}

// checkRateLimitStrict is the fail-closed variant of checkRateLimit for
// credential endpoints (login, setup_admin, reset_password, change_password).
//
// When the rate limiter returns an error (DB unavailable, pool exhaustion), this
// function returns 503 instead of allowing the request through. An attacker who
// can induce DB pressure would otherwise unlock unlimited password guessing on
// these endpoints (02-M1). A high-severity error log is emitted so monitoring
// can detect and alert on the fail-closed window.
//
// A handler with no rate limiter at all is refused the same way (issue #104):
// every production handler is built with one, so a nil limiter is a wiring
// bug, and allowing the request would silently drop brute-force protection.
func (h *Handler) checkRateLimitStrict(ctx context.Context, req *events.LambdaFunctionURLRequest, endpoint string) error {
	if h.rateLimiter == nil {
		logging.Errorf("ALERT: no rate limiter configured for credential endpoint %s; request denied (02-M1)", endpoint)
		return NewClientError(503, "service temporarily unavailable, please try again later")
	}

	clientIP := req.RequestContext.HTTP.SourceIP
	allowed, err := h.rateLimiter.AllowWithIP(ctx, clientIP, endpoint)
	if err != nil {
		// High-severity alert: rate limiter unavailable on a credential endpoint.
		// Fail closed to preserve brute-force protection.
		logging.Errorf("ALERT: rate limiter error on credential endpoint %s for IP %s; request denied (02-M1): %v",
			endpoint, clientIP, err)
		return NewClientError(503, "service temporarily unavailable, please try again later")
	}
	if !allowed {
		logging.Warnf("Rate limit exceeded for %s from IP: %s", endpoint, clientIP)
		return NewClientError(429, "too many requests, please try again later")
	}
	return nil
}

// requireMFASession is requireSession plus the "mfa" bucket applied per IP and
// then per session user, both fail-closed, so a stolen session cannot reset its
// guessing budget by rotating IPs (#94). The session is checked first so
// requests without a valid session are refused with 401 before they reach
// either bucket and cannot spend the per-IP budget of users behind the same
// NAT (#442).
func (h *Handler) requireMFASession(ctx context.Context, req *events.LambdaFunctionURLRequest) (*Session, error) {
	const endpoint = "mfa"
	session, err := h.requireSession(ctx, req)
	if err != nil {
		return nil, err
	}
	// checkRateLimitStrict refuses a nil limiter, so h.rateLimiter is set below.
	if limitErr := h.checkRateLimitStrict(ctx, req, endpoint); limitErr != nil {
		return nil, limitErr
	}
	allowed, err := h.rateLimiter.AllowWithUser(ctx, session.UserID, endpoint)
	if err != nil {
		logging.Errorf("ALERT: rate limiter error on credential endpoint %s for user %s; request denied (02-M1): %v",
			endpoint, session.UserID, err)
		return nil, NewClientError(503, "service temporarily unavailable, please try again later")
	}
	if !allowed {
		logging.Warnf("Rate limit exceeded for %s for user: %s", endpoint, session.UserID)
		return nil, NewClientError(429, "too many requests, please try again later")
	}
	return session, nil
}
