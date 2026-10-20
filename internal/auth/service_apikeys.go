package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	// apiKeyUsageWindow is the length of the request_count_window bucket
	// maintained by PostgresStore.RecordAPIKeyUsage. It is the single source
	// of truth for the window length: the store passes it into the SQL as an
	// interval and the API read path uses it to decide whether a stored
	// window has expired (effectiveWindowUsage), so the write and read sides
	// cannot drift apart.
	apiKeyUsageWindow = 24 * time.Hour

	// apiKeyUsageFlushTimeout bounds a single usage-counter write. The write
	// happens off the auth hot path, so a slow database delays the counter,
	// never the caller's request.
	apiKeyUsageFlushTimeout = 5 * time.Second

	// maxAPIKeyUsageFlushRounds bounds how many times one flush goroutine
	// re-drains a key before handing off. Without it, a key under sustained
	// load would keep a single goroutine writing forever; with it, the
	// leftover count is simply picked up by the next request's flush.
	maxAPIKeyUsageFlushRounds = 16

	// MaxAPIKeyLifetime caps how far in the future ExpiresAt may be set on
	// creation. A key that never expires (nil ExpiresAt) survived
	// indefinitely, including past a password rotation intended to lock out
	// an attacker holding it (issue #102); one year bounds the blast radius
	// of a leaked key to a re-issuance cycle without forcing short-lived
	// CI/CD credentials to be re-minted more often than that.
	MaxAPIKeyLifetime = 365 * 24 * time.Hour
)

// loadActiveUserForAPIKey resolves userID and ensures the account exists and
// is active. Pulled out of CreateAPIKey to keep its cyclomatic complexity
// under the project's gocyclo gate as new checks (e.g. issue #61's empty-
// permissions rejection, issue #102's password/expiry validation) are added.
func (s *Service) loadActiveUserForAPIKey(ctx context.Context, userID string) (*User, error) {
	user, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("user not found")
		}
		return nil, fmt.Errorf("failed to get user: %w", err)
	}
	if user == nil {
		return nil, fmt.Errorf("user not found")
	}
	if !user.Active {
		return nil, fmt.Errorf("user account is not active")
	}
	return user, nil
}

// CreateAPIKey creates a new user API key with scoped permissions. The
// caller's current password must be supplied and is re-verified against the
// stored hash before minting a key: a stolen session token alone must not be
// enough to create a durable, potentially wildcard-scoped credential, the
// same defense-in-depth MFASetup/MFADisable already apply (issue #102).
// Returns the full API key (shown only once), key info, and error.
func (s *Service) CreateAPIKey(ctx context.Context, userID, name, password string, permissions []Permission, expiresAt *time.Time) (string, *UserAPIKey, error) {
	user, err := s.loadActiveUserForAPIKey(ctx, userID)
	if err != nil {
		return "", nil, err
	}

	if err := s.validateCreateAPIKeyRequest(user, password, name, permissions, expiresAt); err != nil {
		return "", nil, err
	}

	// Generate a secure random key (32 bytes = 256 bits)
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		return "", nil, fmt.Errorf("failed to generate random key: %w", err)
	}

	// Base64 encode to create the API key
	apiKey := base64.RawURLEncoding.EncodeToString(keyBytes)

	// Compute SHA-256 hash of the key for storage
	hash := sha256.Sum256([]byte(apiKey))
	keyHash := base64.RawURLEncoding.EncodeToString(hash[:])

	// Extract key prefix (first 8 chars) for display. The key is always 43 chars
	// (32 random bytes base64url-encoded), so [:8] is safe today. The guard makes
	// the function robust to future changes in key-generation length (03-L2).
	keyPrefix := apiKey
	if len(apiKey) > 8 {
		keyPrefix = apiKey[:8]
	}

	// Validate permissions - ensure they don't exceed user's permissions
	if err := s.validateAPIKeyPermissions(ctx, user, permissions); err != nil {
		return "", nil, fmt.Errorf("invalid permissions: %w", err)
	}

	// Create UserAPIKey record
	now := time.Now()
	keyID := uuid.New().String()

	userAPIKey := &UserAPIKey{
		ID:          keyID,
		UserID:      userID,
		Name:        name,
		KeyPrefix:   keyPrefix,
		KeyHash:     keyHash,
		Permissions: permissions,
		ExpiresAt:   expiresAt,
		CreatedAt:   now,
		LastUsedAt:  nil,
		IsActive:    true,
		// The version of the row whose password was just verified, so a
		// rotation that commits before this insert still invalidates the key.
		PasswordVersion: user.PasswordVersion,
	}

	// Store the API key
	if err := s.store.CreateAPIKey(ctx, userAPIKey); err != nil {
		return "", nil, fmt.Errorf("failed to create API key: %w", err)
	}

	logging.Infof("Created API key %s for user %s", keyPrefix, userID)

	return apiKey, userAPIKey, nil
}

// validateCreateAPIKeyRequest checks the caller-supplied fields of a
// CreateAPIKey call: password re-verification, key name, the required
// permissions scope, and the mandatory, capped expiry (issues #61, #102).
// Extracted out of CreateAPIKey to keep its cyclomatic complexity under the
// repo's gocyclo gate.
func (s *Service) validateCreateAPIKeyRequest(user *User, password, name string, permissions []Permission, expiresAt *time.Time) error {
	if !s.verifyPassword(password, user.PasswordHash) {
		return fmt.Errorf("%w", ErrAPIKeyInvalidPassword)
	}
	if name == "" {
		return fmt.Errorf("API key name is required")
	}
	// Reject unscoped keys outright: an unscoped key used to inherit the
	// owner's full permission set (computeEffectivePermissionsFromAuthCtx),
	// which made "leave scope blank" the most permissive option in the UI.
	// Requiring at least one explicit permission forces scoping to be a
	// deliberate choice (issue #61).
	if len(permissions) == 0 {
		return ErrEmptyAPIKeyPermissions
	}
	for i, p := range permissions {
		if err := validateMaxPurchaseAmount(i, p); err != nil {
			return err
		}
	}
	// ExpiresAt is mandatory and capped: a nil value used to mean "never
	// expires", which let a single leaked key outlive any credential rotation.
	if expiresAt == nil {
		return fmt.Errorf("%w", ErrAPIKeyExpiresAtRequired)
	}
	now := time.Now()
	if !expiresAt.After(now) {
		return fmt.Errorf("%w", ErrAPIKeyExpiresAtInPast)
	}
	if expiresAt.After(now.Add(MaxAPIKeyLifetime)) {
		return fmt.Errorf("%w", ErrAPIKeyExpiresAtTooFar)
	}
	return nil
}

// validateAPIKeyPermissions ensures the key's permissions don't exceed the
// user's permissions. Administrators-group members hold {admin, *}, so the
// per-permission HasPermission check below already passes for every requested
// permission; no role-based short-circuit is needed.
func (s *Service) validateAPIKeyPermissions(ctx context.Context, user *User, permissions []Permission) error {
	// Get user's auth context to check their permissions
	authCtx, err := s.GetAuthContext(ctx, user.ID)
	if err != nil {
		return fmt.Errorf("failed to get user permissions: %w", err)
	}

	// Validate each requested permission
	for _, perm := range permissions {
		if !authCtx.HasPermission(perm.Action, perm.Resource) {
			return fmt.Errorf("user does not have permission for action=%s resource=%s", perm.Action, perm.Resource)
		}
	}

	return nil
}

// ListUserAPIKeys retrieves all API keys for a user.
//
// A key minted before the owner's last password change is returned with
// IsActive=false even when its row is still active: ValidateUserAPIKey
// already rejects it, and the row only stays active when the best-effort
// revocation scan after the password change failed (issue #534).
func (s *Service) ListUserAPIKeys(ctx context.Context, userID string) ([]*UserAPIKey, error) {
	// Validate user exists
	user, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("user not found")
		}
		return nil, fmt.Errorf("failed to get user: %w", err)
	}
	if user == nil {
		return nil, fmt.Errorf("user not found")
	}

	keys, err := s.store.ListAPIKeysByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to list API keys: %w", err)
	}

	return markPasswordRotatedKeysInactive(keys, user.PasswordVersion), nil
}

// markPasswordRotatedKeysInactive returns keys with every key minted under a
// password version older than passwordVersion replaced by an inactive copy,
// leaving the store's structs untouched.
//
// It compares with < rather than the != that ValidateUserAPIKey uses because
// the user row is read before the keys: a key minted after a rotation that
// lands between the two reads carries a newer version than this snapshot and
// is valid. password_version only ever increases (migration 000103's trigger),
// so a key older than the snapshot is stale whatever happens after the read.
func markPasswordRotatedKeysInactive(keys []*UserAPIKey, passwordVersion int64) []*UserAPIKey {
	out := make([]*UserAPIKey, len(keys))
	for i, key := range keys {
		out[i] = key
		if key.PasswordVersion < passwordVersion {
			stale := *key
			stale.IsActive = false
			out[i] = &stale
		}
	}
	return out
}

// GetAPIKeyByHash retrieves an API key by its hash (for authentication).
func (s *Service) GetAPIKeyByHash(ctx context.Context, keyHash string) (*UserAPIKey, error) {
	key, err := s.store.GetAPIKeyByHash(ctx, keyHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // treat not-found as nil key; callers check for nil
		}
		return nil, fmt.Errorf("failed to get API key: %w", err)
	}

	return key, nil
}

// authorizeAPIKeyAccess looks up the key and the caller, then returns the key
// together with a permission error if the caller is neither the key owner nor
// an admin. The action string ("revoke" / "delete") is used only in the error
// message so callers get a context-specific message. Pulled out of
// RevokeAPIKey and DeleteAPIKey to deduplicate the identical ownership check
// and keep both functions under the cyclomatic limit.
func (s *Service) authorizeAPIKeyAccess(ctx context.Context, userID, keyID, action string) (*UserAPIKey, error) {
	key, err := s.store.GetAPIKeyByID(ctx, keyID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("API key not found")
		}
		return nil, fmt.Errorf("failed to get API key: %w", err)
	}
	if key == nil {
		return nil, fmt.Errorf("API key not found")
	}

	user, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("user not found")
		}
		return nil, fmt.Errorf("failed to get user: %w", err)
	}
	if user == nil {
		return nil, fmt.Errorf("user not found")
	}

	if key.UserID != userID {
		isAdmin, err := s.UserHasAdminCapability(ctx, userID)
		if err != nil {
			return nil, fmt.Errorf("failed to check admin capability: %w", err)
		}
		if !isAdmin {
			return nil, fmt.Errorf("unauthorized: cannot %s another user's API key", action)
		}
	}

	return key, nil
}

// RevokeAPIKey deactivates an API key (soft delete).
func (s *Service) RevokeAPIKey(ctx context.Context, userID, keyID string) error {
	key, err := s.authorizeAPIKeyAccess(ctx, userID, keyID, "revoke")
	if err != nil {
		return err
	}

	// Revoke the key
	key.IsActive = false
	if err := s.store.UpdateAPIKey(ctx, key); err != nil {
		return fmt.Errorf("failed to revoke API key: %w", err)
	}

	logging.Infof("Revoked API key %s for user %s", key.KeyPrefix, key.UserID)

	return nil
}

// DeleteAPIKey permanently deletes an API key.
func (s *Service) DeleteAPIKey(ctx context.Context, userID, keyID string) error {
	key, err := s.authorizeAPIKeyAccess(ctx, userID, keyID, "delete")
	if err != nil {
		return err
	}

	// Delete the key
	if err := s.store.DeleteAPIKey(ctx, keyID); err != nil {
		return fmt.Errorf("failed to delete API key: %w", err)
	}

	logging.Infof("Deleted API key %s for user %s", key.KeyPrefix, key.UserID)

	return nil
}

// RevokeAllUserAPIKeys deactivates every still-active API key owned by a
// user. Called by ChangePassword and ConfirmPasswordReset alongside
// DeleteUserSessions: a password rotation meant to lock out an attacker
// otherwise left any API key they had minted fully valid, since keys are
// presented via X-API-Key and never touch the session store (issue #102).
//
// Best-effort like the sibling DeleteUserSessions call: a transient store
// failure here is logged by the caller and does not block the password
// change itself, since the new password has already taken effect.
func (s *Service) RevokeAllUserAPIKeys(ctx context.Context, userID string) error {
	keys, err := s.store.ListAPIKeysByUser(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to list API keys for user %s: %w", userID, err)
	}
	for _, key := range keys {
		if !key.IsActive {
			continue
		}
		key.IsActive = false
		if err := s.store.UpdateAPIKey(ctx, key); err != nil {
			return fmt.Errorf("failed to revoke API key %s for user %s: %w", key.ID, userID, err)
		}
	}
	return nil
}

// validateAPIKeyStatus checks that the key is active and not expired.
func validateAPIKeyStatus(key *UserAPIKey) error {
	if !key.IsActive {
		return fmt.Errorf("API key is revoked")
	}
	if key.ExpiresAt != nil && time.Now().After(*key.ExpiresAt) {
		return fmt.Errorf("API key has expired")
	}
	return nil
}

// lookupAPIKeyUser retrieves and validates the user associated with an API key.
func (s *Service) lookupAPIKeyUser(ctx context.Context, userID string) (*User, error) {
	user, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("user account not found")
		}
		return nil, fmt.Errorf("failed to get user: %w", err)
	}
	if user == nil {
		return nil, fmt.Errorf("user account not found")
	}
	if !user.Active {
		return nil, fmt.Errorf("user account is not active")
	}
	return user, nil
}

// ValidateUserAPIKey validates an API key and returns the key info and associated user.
//
// Validation is deliberately side-effect free: it books no usage. A single
// HTTP request validates the same credential several times (authentication
// resolves the principal, then every permission check re-validates, and the
// multi-verb gates re-validate once per verb), so booking here would count
// validations rather than requests and inflate every usage number by a
// per-endpoint factor. Usage is booked exactly once per request by the API
// layer -- see Handler.validateSecurityContext, which calls RecordUsageAsync
// on the one code path guaranteed to run once per request.
func (s *Service) ValidateUserAPIKey(ctx context.Context, apiKey string) (*UserAPIKey, *User, error) {
	hash := sha256.Sum256([]byte(apiKey))
	keyHash := base64.RawURLEncoding.EncodeToString(hash[:])

	key, err := s.GetAPIKeyByHash(ctx, keyHash)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to validate API key: %w", err)
	}
	if key == nil {
		return nil, nil, fmt.Errorf("invalid API key")
	}

	// Constant-time comparison after fetch to close the SQL-equality timing oracle.
	// The DB lookup already found the row by hash; this redundant check ensures the
	// hot path does not leak timing information through early return differences.
	if subtle.ConstantTimeCompare([]byte(key.KeyHash), []byte(keyHash)) != 1 {
		return nil, nil, fmt.Errorf("invalid API key")
	}

	err = validateAPIKeyStatus(key)
	if err != nil {
		return nil, nil, err
	}

	user, err := s.lookupAPIKeyUser(ctx, key.UserID)
	if err != nil {
		return nil, nil, err
	}
	if key.PasswordVersion != user.PasswordVersion {
		return nil, nil, ErrAPIKeyPasswordRotated
	}

	return key, user, nil
}

// RecordUsageAsync books one request against keyID and flushes the pending
// count to the store off the authentication hot path.
//
// Callers must invoke this exactly once per inbound request, not once per
// credential validation -- a single request validates the same key several
// times over. The API layer owns that guarantee (see
// Handler.validateSecurityContext).
//
// The count is accumulated in memory first and the flush writes the whole
// accumulated delta, because singleflight.Group collapses concurrent flushes
// for the same key into one DB write. Incrementing by a fixed 1 inside the
// flush would therefore drop every request that arrived while a write was in
// flight -- a systematic undercount that grows with the key's request rate,
// i.e. worst exactly on the busy keys the usage stats exist to surface.
//
// singleflight is still what bounds the write rate: at most one in-flight DB
// write per keyID at any moment, so a flood of requests on one key cannot
// amplify into a flood of database writes.
func (s *Service) RecordUsageAsync(keyID string) {
	// Book the request BEFORE starting the flush goroutine, so it can never
	// be missed by a flush that is already draining the counter.
	s.addPendingUsage(keyID)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				logging.Warnf("service_apikeys: RecordUsage goroutine panic: %v", r)
			}
		}()

		// singleflight hands a late joiner the in-flight flush's result, and
		// that flush may have drained the counter before this request was
		// booked. Re-check once the shared flush returns and go round again
		// while work remains, so a request is never stranded until the key
		// happens to be used again. Bounded so a key under sustained load
		// hands off instead of pinning one goroutine to the database.
		for range maxAPIKeyUsageFlushRounds {
			if _, sfErr, _ := s.lastUsedSFG.Do(keyID, func() (any, error) {
				s.flushPendingUsage(keyID)
				return nil, nil
			}); sfErr != nil {
				logging.Debugf("service_apikeys: lastUsedSFG returned error for key %s: %v", keyID, sfErr)
				return
			}
			if s.peekPendingUsage(keyID) == 0 {
				return
			}
		}
	}()
}

// addPendingUsage books one not-yet-flushed request against keyID.
//
// Entries are never removed: an entry is one atomic int64 and the number of
// distinct keys is bounded by the number of API keys that exist, so this is
// a fixed small cost rather than unbounded growth. Removing entries would
// race with in-flight increments and risk losing counts.
func (s *Service) addPendingUsage(keyID string) {
	s.pendingUsageMu.Lock()
	if s.pendingUsage == nil {
		s.pendingUsage = make(map[string]*atomic.Int64)
	}
	counter, ok := s.pendingUsage[keyID]
	if !ok {
		counter = new(atomic.Int64)
		s.pendingUsage[keyID] = counter
	}
	s.pendingUsageMu.Unlock()

	counter.Add(1)
}

// pendingCounter returns the counter for keyID, or nil if the key has never
// booked a request. The mutex covers only the map lookup; reads and writes
// of the returned counter are atomic.
func (s *Service) pendingCounter(keyID string) *atomic.Int64 {
	s.pendingUsageMu.Lock()
	defer s.pendingUsageMu.Unlock()
	return s.pendingUsage[keyID]
}

// flushPendingUsage writes the count accumulated for keyID so far to the
// store. A no-op when nothing is pending.
//
// On a store error the drained count is lost rather than retried: the caller
// is a fire-and-forget goroutine on the auth path, and retrying a usage
// counter is not worth holding a database connection during an outage. The
// loss is logged and bounded to the requests in that one flush.
func (s *Service) flushPendingUsage(keyID string) {
	delta := s.drainPendingUsage(keyID)
	if delta == 0 {
		return
	}
	updateCtx, cancel := context.WithTimeout(context.Background(), apiKeyUsageFlushTimeout)
	defer cancel()
	if err := s.RecordUsage(updateCtx, keyID, delta); err != nil {
		logging.Debugf("Failed to record %d API key usage(s) for key %s: %v", delta, keyID, err)
	}
}

// drainPendingUsage atomically takes the pending count for keyID, resetting
// it to zero. Returns 0 when nothing is pending.
func (s *Service) drainPendingUsage(keyID string) int64 {
	counter := s.pendingCounter(keyID)
	if counter == nil {
		return 0
	}
	return counter.Swap(0)
}

// peekPendingUsage reports the pending count for keyID without consuming it.
func (s *Service) peekPendingUsage(keyID string) int64 {
	counter := s.pendingCounter(keyID)
	if counter == nil {
		return 0
	}
	return counter.Load()
}

// UpdateLastUsed updates only the last used timestamp for an API key.
// Retained for backwards compatibility; new code paths should use
// RecordUsage so the request_count_* counters stay current.
func (s *Service) UpdateLastUsed(ctx context.Context, keyID string) error {
	return s.store.UpdateAPIKeyLastUsed(ctx, keyID)
}

// RecordUsage updates last_used_at and adds delta to both the lifetime
// counter and the fixed-window request counter for the key. delta is the
// number of requests being recorded, which is greater than 1 whenever
// concurrent requests were coalesced into one flush. See
// PostgresStore.RecordAPIKeyUsage for the atomic SQL and the window's
// tumbling-reset semantics.
func (s *Service) RecordUsage(ctx context.Context, keyID string, delta int64) error {
	return s.store.RecordAPIKeyUsage(ctx, keyID, delta)
}

// computeEffectivePermissionsFromAuthCtx returns the subset of key permissions
// that the owner's authCtx also grants at the action/resource level, keeping
// the key's own constraint limits. If the key has no specific permissions it
// authorizes nothing: an unscoped key authenticates the owner but grants no
// permissions (deny-by-default, issue #61), rather than inheriting the
// owner's full permission set. That inheritance was a privilege-escalation
// trap -- a long-lived unscoped key silently became admin-capable the moment
// its owner joined the Administrators group. The result carries the key's
// constraints, not the owner's; callers that need both sources must check
// ownerAuthCtx.Permissions independently.
func computeEffectivePermissionsFromAuthCtx(key *UserAPIKey, authCtx *AuthContext) []Permission {
	if len(key.Permissions) == 0 {
		return []Permission{}
	}
	effectivePerms := make([]Permission, 0, len(key.Permissions))
	for _, keyPerm := range key.Permissions {
		if authCtx.HasPermission(keyPerm.Action, keyPerm.Resource) {
			effectivePerms = append(effectivePerms, keyPerm)
		}
	}
	return effectivePerms
}

// ComputeEffectivePermissions computes the intersection of API key permissions and user permissions.
// This ensures an API key cannot grant more permissions than the user has.
//
// An unscoped key (no key-specific permissions) grants nothing, regardless of
// the owner's group; see computeEffectivePermissionsFromAuthCtx. A scoped
// admin key's permissions all pass the HasPermission intersection below, so
// the group-derived path still lets a deliberately-admin-scoped key act with
// the owner's {admin, *} authority.
func (s *Service) ComputeEffectivePermissions(ctx context.Context, apiKey *UserAPIKey, user *User) ([]Permission, error) {
	authCtx, err := s.GetAuthContext(ctx, user.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to get user auth context: %w", err)
	}
	return computeEffectivePermissionsFromAuthCtx(apiKey, authCtx), nil
}
