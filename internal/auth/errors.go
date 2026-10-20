package auth

import "errors"

// Validation sentinels — wrapped by service_user.go's validators so the
// API handler can map each to a precise HTTP status code. Plain
// fmt.Errorf returns fall through to a generic 500 in
// internal/api/handler.go's handleRequestError, which hides the real
// cause from the user (see issue #349).
//
// Callers use errors.Is to detect the category; the wrapped message
// (when set via fmt.Errorf("%w: %s", ...)) carries the specific user-
// facing detail (e.g. "invalid role: guest", "password does not meet
// policy: must be at least 12 characters").
var (
	ErrInvalidEmail   = errors.New("invalid email format")
	ErrEmailInUse     = errors.New("email already in use")
	ErrAdminExists    = errors.New("admin user already exists")
	ErrPasswordPolicy = errors.New("password does not meet policy")

	// ErrNoGroups is returned when a create/update would leave a user with
	// zero group memberships. Authorization derives entirely from groups, so
	// a zero-group user can do nothing; the API rejects it as a 400 rather
	// than silently creating an inert account (issue #907).
	ErrNoGroups = errors.New("user must belong to at least one group")

	// ErrLastAdmin is returned when an update or delete would remove the last
	// remaining member of the Administrators group, which would lock everyone
	// out of admin-gated functionality. Mapped to 409 (issue #907).
	ErrLastAdmin = errors.New("cannot remove the last administrator")

	// ErrSelfEscalation is returned when a user attempts to grant themselves
	// a group they are not already a member of without holding the manage-users
	// permission. Mapped to 403 (issue #907).
	ErrSelfEscalation = errors.New("cannot escalate your own group membership")

	// ErrPermissionCeiling is returned by a group write that would grant a
	// permission the acting user does not themselves hold (or holds only at
	// a narrower constraint scope), and by the fail-closed paths where the
	// acting user's own permissions cannot be resolved at all. Mapped to 403
	// (issue #1550).
	ErrPermissionCeiling = errors.New("permission ceiling exceeded")

	// ErrPermissionNotGrantable is returned by a group write that would ADD
	// one of the money-spending verbs carved out of the admin:* wildcard
	// (adminCarvedOuts). Those are reserved for separation of duties and are
	// provisioned by migration, never through the API. Mapped to 403
	// (issues #923, #1550).
	ErrPermissionNotGrantable = errors.New("permission not grantable")

	// ErrInvalidPermission is returned when a group write carries a
	// permission entry with a blank action or resource. A blank resource is
	// malformed input, NOT a request for the "*" wildcard: the group-edit
	// form renders an empty stored resource as the selected "All (*)" option
	// and saves it back as view:*, and the same widening is reachable
	// directly through the API because nothing validated the list. Also
	// returned for a negative or NaN MaxPurchaseAmount on a group or API-key
	// write (issue #83). Mapped to 400 (issues #1730, #1550).
	ErrInvalidPermission = errors.New("invalid permission")

	// ErrSystemManagedGroup is returned when a write targets one of the
	// seeded, system-managed groups. Their contents are owned by migrations;
	// an API edit that reshapes them can silently disable a whole capability
	// tenant-wide. Mapped to 403 (issue #1629).
	ErrSystemManagedGroup = errors.New("system-managed group cannot be modified")

	// ErrEmptyPermissions is returned when a group update explicitly sends
	// permissions as an empty list. APIUpdateGroupRequest.Permissions is a
	// pointer so a nil value (field omitted) can be told apart from a
	// pointer to an empty slice (field sent as `[]`): the former means
	// "leave unchanged" (this endpoint has no other way to express that),
	// the latter must be refused rather than silently treated as a no-op
	// that reports success while the group's real permissions -- possibly
	// admin:* -- are left untouched (issue #237). Mapped to 400.
	ErrEmptyPermissions = errors.New("a group must grant at least one permission; delete the group to remove all privileges")

	// ErrCurrentPasswordIncorrect is returned by UpdateUserProfile when the
	// caller-supplied current password does not match the stored hash. Mapped
	// to 401 at the API layer (the acting user is verifying their own
	// credential, so a precise message is safe -- issue #929).
	ErrCurrentPasswordIncorrect = errors.New("current password is incorrect")

	// ErrEmptyAPIKeyPermissions is returned by CreateAPIKey when the request
	// carries zero permissions. An unscoped key used to silently inherit the
	// owner's full permission set at request time; requiring at least one
	// explicit permission at creation forces scoping to be deliberate rather
	// than the most-permissive default. Mapped to 400 (issue #61).
	ErrEmptyAPIKeyPermissions = errors.New("at least one permission is required")

	// ErrInvalidResetToken: the reset token is unknown, already used, or
	// belongs to an admin-deactivated account (A03-006). The deactivated case
	// shares this error so the reset endpoint never reveals account state
	// (issue #595). Mapped to 400.
	ErrInvalidResetToken = errors.New("invalid or expired reset token")

	// ErrUserChanged: a field-scoped user write found the row changed since the
	// caller read it (issue #474). Mapped to 409.
	ErrUserChanged = errors.New("account changed concurrently; reload and retry")

	// MFA login-gate sentinels — used by the login API handler to map
	// to machine-readable response codes (mfa_required /
	// invalid_mfa_code) so the frontend can branch on the error class
	// without substring-matching the human message. See issue #497.
	ErrMFARequired    = errors.New("mfa_required")
	ErrInvalidMFACode = errors.New("invalid_mfa_code")

	// MFA service-operation sentinels — returned (wrapped via fmt.Errorf
	// "%w") by the MFA lifecycle methods in service_mfa.go so the API
	// handler can map each error class to the right HTTP status code via
	// errors.Is rather than brittle substring matching. See issue #512.
	//
	// ErrMFAInvalidPassword        — wrong current password on setup/disable.
	// ErrMFAInvalidCode            — wrong TOTP or recovery code.
	// ErrMFACodeRequired           — MFA-enabled user supplied no code on disable.
	// ErrMFANoEnrollmentInProgress — MFAEnable called before MFASetup.
	// ErrMFAEnrollmentExpired      — pending enrollment window elapsed.
	// ErrMFANotEnabled             — regenerate/disable called when MFA is off.
	// ErrMFAAuthFailed             — generic opaque auth failure (user not found or
	//                               DB error; maps to 401 to prevent user enumeration).
	//
	// Message strings are intentionally identical to the pre-sentinel
	// fmt.Errorf literals so that existing tests relying on err.Error()
	// substrings continue to pass unchanged. See issue #512.
	ErrMFAInvalidPassword        = errors.New("invalid password")
	ErrMFAInvalidCode            = errors.New("invalid MFA code")
	ErrMFACodeRequired           = errors.New("MFA code or recovery code required")
	ErrMFANoEnrollmentInProgress = errors.New("no MFA enrollment in progress")
	ErrMFAEnrollmentExpired      = errors.New("MFA enrollment expired")
	ErrMFANotEnabled             = errors.New("MFA is not enabled")
	ErrMFAAuthFailed             = errors.New("authentication failed")

	// ErrMFAAlreadyEnabled: setup or enable on an account whose MFA is on.
	// Replacing a factor needs the disable proof first (issue #227). Mapped to 409.
	ErrMFAAlreadyEnabled = errors.New("MFA is already enabled; disable it before enrolling a new authenticator")

	// API key creation sentinels — returned (wrapped via fmt.Errorf "%w") by
	// CreateAPIKey so the API handler can map each to the right HTTP status
	// via errors.Is rather than substring matching. See issue #102.
	//
	// ErrAPIKeyInvalidPassword — wrong current password on key creation
	//                            (same defense-in-depth re-verification as
	//                            MFASetup/MFADisable).
	// ErrAPIKeyExpiresAtRequired — no expires_at supplied; unscoped-lifetime
	//                              keys are no longer allowed.
	// ErrAPIKeyExpiresAtTooFar  — expires_at exceeds MaxAPIKeyLifetime.
	// ErrAPIKeyExpiresAtInPast  — expires_at is not in the future, which would
	//                             mint a key that is already expired.
	ErrAPIKeyInvalidPassword   = errors.New("invalid password")
	ErrAPIKeyExpiresAtRequired = errors.New("expires_at is required")
	ErrAPIKeyExpiresAtTooFar   = errors.New("expires_at exceeds the maximum API key lifetime")
	ErrAPIKeyExpiresAtInPast   = errors.New("expires_at must be in the future")

	// ErrAPIKeyPasswordRotated: the owner's password changed after the key was
	// minted (issue #402).
	ErrAPIKeyPasswordRotated = errors.New("API key predates the owner's last password change")

	// ErrSessionPasswordRotated: the user's password changed after the session
	// was issued (issue #398).
	ErrSessionPasswordRotated = errors.New("session predates the user's last password change")
)
