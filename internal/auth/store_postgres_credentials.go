package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// UpdateUserAdminFields writes only the columns an admin edit changes (email,
// group membership, active state and its deactivation stamp), and only while
// the row still holds the email, groups and active state the caller read, so a
// stale admin save cannot erase MFA fields or restore an old password hash
// (issue #493).
//
// The write can deactivate a member or drop the Administrators group, so it
// takes the admin-invariant lock first (see adminInvariantAdvisoryLockKey).
func (s *PostgresStore) UpdateUserAdminFields(ctx context.Context, user *User, readEmail string, readGroupIDs []string, readActive bool) error {
	return s.withAdminInvariantLock(ctx, "update user", func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			UPDATE users SET email = $2, group_ids = $3, active = $4, deactivated_at = $5, updated_at = NOW()
			WHERE id = $1 AND email = $6 AND group_ids = $7 AND active = $8
			RETURNING updated_at
		`, user.ID, user.Email, user.GroupIDs, user.Active, user.DeactivatedAt, readEmail, readGroupIDs, readActive).Scan(&user.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserChanged
		}
		if err != nil {
			return fmt.Errorf("failed to update user: %w", err)
		}
		return nil
	})
}

// UpdateUserCredentials writes only the email and password columns, and only
// while the row still holds the email and hash the caller read (issue #474).
// It also clears any outstanding reset token so a stale link cannot overwrite
// the credentials written here (issue #526).
func (s *PostgresStore) UpdateUserCredentials(ctx context.Context, user *User, readEmail, readPasswordHash string) error {
	result, err := s.db.Exec(ctx, `
		UPDATE users SET email = $4, password_hash = $5,
			password_history = $6, password_reset_token = NULL, password_reset_expiry = NULL,
			updated_at = NOW()
		WHERE id = $1 AND email = $2 AND password_hash = $3
	`, user.ID, readEmail, readPasswordHash, user.Email, user.PasswordHash, user.PasswordHistory)
	if err != nil {
		return fmt.Errorf("failed to update user credentials: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrUserChanged
	}
	return nil
}

// CompletePasswordReset writes the new password, activates the account and
// consumes the reset token, only while the row still holds the token and hash
// the caller read, the token has not expired and the account is not
// deactivated (issues #493, #526).
func (s *PostgresStore) CompletePasswordReset(ctx context.Context, user *User, readResetToken, readPasswordHash string) error {
	result, err := s.db.Exec(ctx, `
		UPDATE users SET password_hash = $4, password_history = $5, active = $6,
			password_reset_token = NULL, password_reset_expiry = NULL, updated_at = NOW()
		WHERE id = $1 AND password_reset_token = $2 AND password_hash = $3 AND deactivated_at IS NULL
			AND password_reset_expiry > NOW()
	`, user.ID, readResetToken, readPasswordHash, user.PasswordHash, user.PasswordHistory, user.Active)
	if err != nil {
		return fmt.Errorf("failed to complete password reset: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrUserChanged
	}
	return nil
}

// ConsumePasswordResetToken clears the reset token columns only while the row
// still holds the token the caller read.
func (s *PostgresStore) ConsumePasswordResetToken(ctx context.Context, userID, readResetToken string) error {
	result, err := s.db.Exec(ctx, `
		UPDATE users SET password_reset_token = NULL, password_reset_expiry = NULL, updated_at = NOW()
		WHERE id = $1 AND password_reset_token = $2
	`, userID, readResetToken)
	if err != nil {
		return fmt.Errorf("failed to consume reset token: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrUserChanged
	}
	return nil
}

// ConsumeMFARecoveryCode writes only the recovery codes, and only while the row
// still holds the codes the caller read, so each code is spent once (issue #493).
func (s *PostgresStore) ConsumeMFARecoveryCode(ctx context.Context, userID string, readCodes, remaining []string) error {
	result, err := s.db.Exec(ctx, `
		UPDATE users SET mfa_recovery_codes = $3, updated_at = NOW()
		WHERE id = $1 AND mfa_recovery_codes = $2
	`, userID, readCodes, remaining)
	if err != nil {
		return fmt.Errorf("failed to consume recovery code: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrUserChanged
	}
	return nil
}

// SetPendingMFASecret writes only the pending enrollment, and only while MFA is
// off, so a stale setup cannot overwrite a concurrent enrollment (issue #227).
func (s *PostgresStore) SetPendingMFASecret(ctx context.Context, userID, secret string, expiresAt time.Time) error {
	result, err := s.db.Exec(ctx, `
		UPDATE users SET mfa_pending_secret = $2, mfa_pending_secret_expires_at = $3, updated_at = NOW()
		WHERE id = $1 AND mfa_enabled = false
	`, userID, secret, expiresAt)
	if err != nil {
		return fmt.Errorf("failed to persist pending MFA secret: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrUserChanged
	}
	return nil
}

// EnableMFA promotes the pending secret the caller verified, only while MFA is
// still off and that secret is still the pending one and unexpired (issues #227,
// #527). It also sets the last accepted TOTP counter to the enrollment code's,
// so that code cannot be spent again at Login (#442). The counter is replaced,
// not raised: it was recorded for the previous secret, whose codes no longer
// verify, and keeping a higher one would refuse a fresh code of the new secret.
func (s *PostgresStore) EnableMFA(ctx context.Context, userID, pendingSecret string, totpCounter int64, recoveryHashes []string) error {
	result, err := s.db.Exec(ctx, `
		UPDATE users SET mfa_enabled = true, mfa_secret = $2, mfa_recovery_codes = $3,
			mfa_last_totp_counter = $4,
			mfa_pending_secret = '', mfa_pending_secret_expires_at = NULL, updated_at = NOW()
		WHERE id = $1 AND mfa_enabled = false AND mfa_pending_secret = $2
			AND mfa_pending_secret_expires_at > NOW()
	`, userID, pendingSecret, recoveryHashes, totpCounter)
	if err != nil {
		return fmt.Errorf("failed to enable MFA: %w", err)
	}
	if result.RowsAffected() == 0 {
		return s.enableMFARefusal(ctx, userID, pendingSecret)
	}
	return nil
}

// enableMFARefusal names why EnableMFA's guarded write matched no row: the
// enrollment expired if MFA is still off under the same pending secret and its
// expiry has passed on the database clock, otherwise the row changed (issue
// #568). Pending secrets are random and leave the row only by being cleared or
// replaced, so a row still holding this one was refused by the expiry check.
// If the check itself fails, the write's own verdict, ErrUserChanged, stands.
func (s *PostgresStore) enableMFARefusal(ctx context.Context, userID, pendingSecret string) error {
	var expired bool
	err := s.db.QueryRow(ctx, `
		SELECT (mfa_pending_secret_expires_at <= NOW()) IS TRUE FROM users
		WHERE id = $1 AND mfa_enabled = false AND mfa_pending_secret = $2
	`, userID, pendingSecret).Scan(&expired)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrUserChanged
	case err != nil:
		return fmt.Errorf("%w (checking enrollment expiry: %w)", ErrUserChanged, err)
	case expired:
		return ErrMFAEnrollmentExpired
	}
	return ErrUserChanged
}

// ReplaceMFARecoveryCodes writes only the recovery codes, and only while MFA is
// on under the secret and codes the caller read (issue #527).
func (s *PostgresStore) ReplaceMFARecoveryCodes(ctx context.Context, userID, readSecret string, readCodes, newHashes []string) error {
	result, err := s.db.Exec(ctx, `
		UPDATE users SET mfa_recovery_codes = $4, updated_at = NOW()
		WHERE id = $1 AND mfa_enabled = true AND mfa_secret = $2 AND mfa_recovery_codes = $3
	`, userID, readSecret, readCodes, newHashes)
	if err != nil {
		return fmt.Errorf("failed to replace recovery codes: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrUserChanged
	}
	return nil
}

// DisableMFA clears only the MFA columns, and only while MFA is on under the
// secret and codes the caller read, so a recovery code spent in between is not
// restored and no other column is rewritten (issue #527).
func (s *PostgresStore) DisableMFA(ctx context.Context, userID, readSecret string, readCodes []string) error {
	result, err := s.db.Exec(ctx, `
		UPDATE users SET mfa_enabled = false, mfa_secret = '', mfa_recovery_codes = '{}',
			mfa_pending_secret = '', mfa_pending_secret_expires_at = NULL, updated_at = NOW()
		WHERE id = $1 AND mfa_enabled = true AND mfa_secret = $2 AND mfa_recovery_codes = $3
	`, userID, readSecret, readCodes)
	if err != nil {
		return fmt.Errorf("failed to disable MFA: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrUserChanged
	}
	return nil
}

// ClaimTOTPCounter advances the last accepted TOTP counter only while the stored
// one is lower, so a code is accepted once even when two requests race (issue #442).
func (s *PostgresStore) ClaimTOTPCounter(ctx context.Context, userID string, counter int64) (bool, error) {
	result, err := s.db.Exec(ctx, `
		UPDATE users SET mfa_last_totp_counter = $2
		WHERE id = $1 AND mfa_last_totp_counter < $2
	`, userID, counter)
	if err != nil {
		return false, fmt.Errorf("failed to record TOTP counter: %w", err)
	}
	return result.RowsAffected() == 1, nil
}

// ClearPendingMFASecret cancels a pending enrollment, only while MFA is off, so
// a password-only cancel cannot erase a concurrent enrollment (issue #227).
func (s *PostgresStore) ClearPendingMFASecret(ctx context.Context, userID string) error {
	result, err := s.db.Exec(ctx, `
		UPDATE users SET mfa_pending_secret = '', mfa_pending_secret_expires_at = NULL, updated_at = NOW()
		WHERE id = $1 AND mfa_enabled = false
	`, userID)
	if err != nil {
		return fmt.Errorf("failed to clear pending MFA secret: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrUserChanged
	}
	return nil
}

// SetPasswordResetToken writes only the reset token columns, and only while the
// account is still active under the same email with the reset expiry the caller
// read, so a concurrent deactivation or reset issuance wins.
func (s *PostgresStore) SetPasswordResetToken(ctx context.Context, user *User, readExpiry *time.Time) error {
	result, err := s.db.Exec(ctx, `
		UPDATE users SET password_reset_token = $3, password_reset_expiry = $4, updated_at = NOW()
		WHERE id = $1 AND email = $2 AND deactivated_at IS NULL
			AND password_reset_expiry IS NOT DISTINCT FROM $5
	`, user.ID, user.Email, user.PasswordResetToken, user.PasswordResetExpiry, readExpiry)
	if err != nil {
		return fmt.Errorf("failed to save reset token: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrUserChanged
	}
	return nil
}
