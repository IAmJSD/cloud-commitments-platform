package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/jackc/pgx/v5"
)

// ==========================================
// SESSION OPERATIONS
// ==========================================

// CreateSession creates a new session.
func (s *PostgresStore) CreateSession(ctx context.Context, session *Session) error {
	query := `
		INSERT INTO sessions (
			token, user_id, email, expires_at, created_at,
			user_agent, ip_address, csrf_token, password_version
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`

	_, err := s.db.Exec(ctx, query,
		session.Token,
		session.UserID,
		session.Email,
		session.ExpiresAt,
		session.CreatedAt,
		session.UserAgent,
		session.IPAddress,
		session.CSRFToken,
		session.PasswordVersion,
	)

	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}

	return nil
}

// GetSession retrieves a session by token.
func (s *PostgresStore) GetSession(ctx context.Context, token string) (*Session, error) {
	query := `
		SELECT token, user_id, email, expires_at, created_at,
		       user_agent, ip_address, csrf_token, password_version
		FROM sessions
		WHERE token = $1 AND expires_at > NOW()
	`

	var session Session
	err := s.db.QueryRow(ctx, query, token).Scan(
		&session.Token,
		&session.UserID,
		&session.Email,
		&session.ExpiresAt,
		&session.CreatedAt,
		&session.UserAgent,
		&session.IPAddress,
		&session.CSRFToken,
		&session.PasswordVersion,
	)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("session not found or expired")
		}
		return nil, fmt.Errorf("failed to get session: %w", err)
	}

	return &session, nil
}

// DeleteSession deletes a session.
func (s *PostgresStore) DeleteSession(ctx context.Context, token string) error {
	query := `DELETE FROM sessions WHERE token = $1`

	_, err := s.db.Exec(ctx, query, token)
	if err != nil {
		return fmt.Errorf("failed to delete session: %w", err)
	}

	return nil
}

// DeleteUserSessions deletes all sessions for a user.
func (s *PostgresStore) DeleteUserSessions(ctx context.Context, userID string) error {
	query := `DELETE FROM sessions WHERE user_id = $1`

	_, err := s.db.Exec(ctx, query, userID)
	if err != nil {
		return fmt.Errorf("failed to delete user sessions: %w", err)
	}

	return nil
}

// CleanupExpiredSessions deletes expired sessions.
func (s *PostgresStore) CleanupExpiredSessions(ctx context.Context) error {
	query := `DELETE FROM sessions WHERE expires_at <= NOW()`

	result, err := s.db.Exec(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to cleanup expired sessions: %w", err)
	}

	logging.Debugf("Cleaned up %d expired sessions", result.RowsAffected())
	return nil
}
