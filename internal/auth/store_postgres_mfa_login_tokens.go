package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ==========================================
// MFA LOGIN TOKEN OPERATIONS (issue #134)
// ==========================================

// CreateMFALoginToken stores a new MFA login token and sweeps expired ones.
// The expiry comes from the database clock, the same clock the claim and
// consume statements compare against.
func (s *PostgresStore) CreateMFALoginToken(ctx context.Context, tokenHash, userID string, passwordVersion int64, ttl time.Duration) (time.Time, error) {
	if _, err := s.db.Exec(ctx, `DELETE FROM mfa_login_tokens WHERE expires_at <= NOW()`); err != nil {
		return time.Time{}, fmt.Errorf("failed to sweep expired MFA login tokens: %w", err)
	}
	var expiresAt time.Time
	err := s.db.QueryRow(ctx, `
		INSERT INTO mfa_login_tokens (token_hash, user_id, password_version, expires_at)
		VALUES ($1, $2, $3, NOW() + make_interval(secs => $4))
		RETURNING expires_at
	`, tokenHash, userID, passwordVersion, ttl.Seconds()).Scan(&expiresAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to create MFA login token: %w", err)
	}
	return expiresAt, nil
}

// ClaimMFALoginTokenAttempt spends one attempt in a single UPDATE. The row
// lock serializes concurrent claims and READ COMMITTED re-checks the WHERE
// clause against the committed count, so at most maxAttempts claims succeed
// per token however many callers race.
func (s *PostgresStore) ClaimMFALoginTokenAttempt(ctx context.Context, tokenHash string, maxAttempts int) (*MFALoginToken, error) {
	var t MFALoginToken
	err := s.db.QueryRow(ctx, `
		UPDATE mfa_login_tokens SET attempts = attempts + 1
		WHERE token_hash = $1 AND expires_at > NOW() AND attempts < $2
		RETURNING user_id, password_version, attempts, expires_at
	`, tokenHash, maxAttempts).Scan(&t.UserID, &t.PasswordVersion, &t.Attempts, &t.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to claim MFA login token attempt: %w", err)
	}
	return &t, nil
}

// ConsumeMFALoginToken deletes the token. Of two concurrent callers only one
// sees the row deleted, so only one login completes per token.
func (s *PostgresStore) ConsumeMFALoginToken(ctx context.Context, tokenHash string) (bool, error) {
	result, err := s.db.Exec(ctx, `
		DELETE FROM mfa_login_tokens WHERE token_hash = $1 AND expires_at > NOW()
	`, tokenHash)
	if err != nil {
		return false, fmt.Errorf("failed to consume MFA login token: %w", err)
	}
	return result.RowsAffected() == 1, nil
}
