package auth

import (
	"context"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
)

// MFA login continuation tokens (issue #134).
//
// A correct password for an MFA-enrolled user used to answer only
// mfa_required, so the browser held the plaintext password until the user
// typed the code and resent password + code. The answer now also carries a
// server-issued mfa_token, and the code step can send
// {email, mfa_token, mfa_code} in place of the password. The password + code
// form keeps working until the frontend switches.
const (
	// MFALoginTokenTTL is how long the code step may follow the password step.
	MFALoginTokenTTL = 5 * time.Minute

	// MaxMFALoginTokenAttempts is how many code checks one token allows.
	// Kept below MaxFailedLoginAttempts so a user who mistypes the code a
	// few times is sent back to the password step before the account locks.
	MaxMFALoginTokenAttempts = 3
)

// MFALoginToken is the stored state of a pending MFA login. Only the hash of
// the token is stored; the raw value goes to the client once.
type MFALoginToken struct {
	UserID string
	// PasswordVersion is the users.password_version the password step read;
	// a password change before the code step voids the token.
	PasswordVersion int64
	Attempts        int
	ExpiresAt       time.Time
}

// MFARequiredError is the mfa_required answer with its continuation token.
// errors.Is(err, ErrMFARequired) holds for it.
type MFARequiredError struct {
	Token     string
	ExpiresAt time.Time
}

func (e *MFARequiredError) Error() string { return ErrMFARequired.Error() }

// Unwrap lets errors.Is match ErrMFARequired.
func (e *MFARequiredError) Unwrap() error { return ErrMFARequired }

// issueMFAChallenge mints the mfa_token for a user whose password just
// verified. If the token cannot be stored it answers a bare ErrMFARequired,
// which leaves the password + code form as the way in.
func (s *Service) issueMFAChallenge(ctx context.Context, user *User) error {
	rawToken, err := generateToken()
	if err != nil {
		logging.Warnf("Failed to generate MFA login token for user %s: %v", user.ID, err)
		return ErrMFARequired
	}
	expiresAt, err := s.store.CreateMFALoginToken(ctx, hashSessionToken(rawToken), user.ID, user.PasswordVersion, MFALoginTokenTTL)
	if err != nil {
		logging.Warnf("Failed to store MFA login token for user %s: %v", user.ID, err)
		return ErrMFARequired
	}
	return &MFARequiredError{Token: rawToken, ExpiresAt: expiresAt}
}

// loginWithMFAToken completes a login from an mfa_token and a code.
//
// Every request that presents a live token spends one of its attempts before
// anything else is checked, in one atomic UPDATE, so concurrent requests
// cannot check more than MaxMFALoginTokenAttempts codes per token. The token
// is bound to the account it was issued for (req.Email must name it) and to
// that account's password version. A wrong code also counts toward the
// account lockout, as on the password + code form. Only the request that
// deletes the token gets a session, so a token logs in at most once.
func (s *Service) loginWithMFAToken(ctx context.Context, req LoginRequest) (*LoginResponse, error) {
	if req.Password != "" || req.MFACode == "" {
		return nil, ErrInvalidMFAToken
	}
	tokenHash := hashSessionToken(req.MFAToken)
	user, err := s.claimMFALoginToken(ctx, tokenHash, req.Email)
	if err != nil {
		return nil, err
	}

	if codeErr := s.verifyLoginMFACode(ctx, user, req.MFACode); codeErr != nil {
		return nil, codeErr
	}

	consumed, err := s.store.ConsumeMFALoginToken(ctx, tokenHash)
	if err != nil {
		logging.Warnf("Failed to consume MFA login token for user %s: %v", user.ID, err)
		return nil, ErrInvalidMFAToken
	}
	if !consumed {
		return nil, ErrInvalidMFAToken
	}
	return s.completeSuccessfulLogin(ctx, user)
}

// claimMFALoginToken spends one attempt on the token and returns its user,
// provided email names that user, the account may still sign in, MFA is
// still on and the password has not changed since the token was issued.
func (s *Service) claimMFALoginToken(ctx context.Context, tokenHash, email string) (*User, error) {
	claim, err := s.store.ClaimMFALoginTokenAttempt(ctx, tokenHash, MaxMFALoginTokenAttempts)
	if err != nil {
		logging.Warnf("Failed to claim MFA login token attempt: %v", err)
		return nil, ErrInvalidMFAToken
	}
	if claim == nil {
		return nil, ErrInvalidMFAToken
	}
	user, err := s.getUserAndValidateStatus(ctx, email)
	if err != nil {
		return nil, err
	}
	if user.ID != claim.UserID || !user.MFAEnabled || user.PasswordVersion != claim.PasswordVersion {
		return nil, ErrInvalidMFAToken
	}
	return user, nil
}
