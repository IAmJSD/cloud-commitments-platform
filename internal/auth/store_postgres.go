package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DBConnection defines the interface for database operations needed by PostgresStore.
type DBConnection interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Begin(ctx context.Context) (pgx.Tx, error)
	Ping(ctx context.Context) error
}

// PostgresStore implements StoreInterface using PostgreSQL.
type PostgresStore struct {
	db DBConnection
}

// NewPostgresStore creates a new PostgreSQL-backed auth store.
func NewPostgresStore(db DBConnection) *PostgresStore {
	return &PostgresStore{db: db}
}

// Verify PostgresStore implements StoreInterface.
var _ StoreInterface = (*PostgresStore)(nil)

// ==========================================
// USER OPERATIONS
// ==========================================

// GetUserByID retrieves a user by ID.
func (s *PostgresStore) GetUserByID(ctx context.Context, userID string) (*User, error) {
	query := `
		SELECT ` + userSelectColumns + `
		FROM users
		WHERE id = $1
	`

	return s.scanUser(s.db.QueryRow(ctx, query, userID))
}

// GetUserByEmail retrieves a user by email.
func (s *PostgresStore) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	query := `
		SELECT ` + userSelectColumns + `
		FROM users
		WHERE email = $1
	`

	user, err := s.scanUser(s.db.QueryRow(ctx, query, email))
	if err != nil {
		return nil, err
	}
	return user, nil
}

// CreateUser creates a new user.
func (s *PostgresStore) CreateUser(ctx context.Context, user *User) error {
	// Generate UUID if not provided
	if user.ID == "" {
		user.ID = uuid.New().String()
	}

	// Set timestamps
	now := time.Now()
	user.CreatedAt = now
	user.UpdatedAt = now

	query := `
		INSERT INTO users (` + userInsertColumns + `)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
	`

	_, err := s.db.Exec(ctx, query, userInsertArgs(user, user.GroupIDs)...)

	if err != nil {
		if isEmailDuplicateError(err) {
			return fmt.Errorf("email already in use")
		}
		if isDuplicateKeyError(err) {
			return fmt.Errorf("failed to create user: unique constraint violation: %w", err)
		}
		return fmt.Errorf("failed to create user: %w", err)
	}

	return nil
}

// DeleteUser deletes a user. Deleting an Administrators-group member lowers
// the admin count, so it takes the admin-invariant lock first (see
// adminInvariantAdvisoryLockKey).
func (s *PostgresStore) DeleteUser(ctx context.Context, userID string) error {
	query := `DELETE FROM users WHERE id = $1`

	return s.withAdminInvariantLock(ctx, "delete user", func(tx pgx.Tx) error {
		result, err := tx.Exec(ctx, query, userID)
		if err != nil {
			return fmt.Errorf("failed to delete user: %w", err)
		}
		if result.RowsAffected() == 0 {
			return fmt.Errorf("user not found: %s", userID)
		}
		return nil
	})
}

// ListUsers lists all users.
func (s *PostgresStore) ListUsers(ctx context.Context) ([]User, error) {
	// LIMIT provides a safety cap against unbounded memory allocation on large installations.
	// Pagination support should be added if this limit proves insufficient.
	query := `
		SELECT ` + userSelectColumns + `
		FROM users
		ORDER BY created_at DESC
		LIMIT 10000
	`

	rows, err := s.db.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to list users: %w", err)
	}
	defer rows.Close()

	users := make([]User, 0)
	for rows.Next() {
		user, err := s.scanUser(rows)
		if err != nil {
			return nil, err
		}
		if user == nil {
			continue
		}
		users = append(users, *user)
	}

	return users, rows.Err()
}

// GetUserByResetToken retrieves a user by password reset token without
// filtering on expiry. Both callers (ResetTokenStatus and
// validateResetToken) perform their own expiry check on the returned
// row, so the SQL must surface expired rows; otherwise an expired
// token returns pgx.ErrNoRows and ResetTokenStatus misclassifies it
// as "used" instead of "expired" (QA bug 11.2).
func (s *PostgresStore) GetUserByResetToken(ctx context.Context, token string) (*User, error) {
	query := `
		SELECT ` + userSelectColumns + `
		FROM users
		WHERE password_reset_token = $1
	`

	return s.scanUser(s.db.QueryRow(ctx, query, token))
}

// ==========================================
// HELPER FUNCTIONS
// ==========================================

// userInsertColumns lists the users columns that CreateUser and
// CreateAdminIfNone write, in the order userInsertArgs returns their values.
// password_version and deactivated_at are left to their column defaults.
const userInsertColumns = `id, email, password_hash, group_ids, active,
	mfa_enabled, mfa_secret, mfa_pending_secret, mfa_pending_secret_expires_at,
	mfa_recovery_codes, password_reset_token, password_reset_expiry,
	failed_login_attempts, locked_until, password_history,
	created_at, updated_at, last_login_at`

// userSelectColumns lists the users columns every user SELECT reads, in the
// order scanUser scans them. Every query that feeds scanUser must select
// exactly this list: several neighbors share a Go type (mfa_secret and
// mfa_pending_secret, or the timestamps), so a hand-copied list in a
// different order would scan one into the other without any error (#313).
const userSelectColumns = userInsertColumns + `, deactivated_at, password_version`

// userInsertArgs returns the values for userInsertColumns, in the same order.
// groupIDs is passed separately because CreateAdminIfNone forces the
// Administrators group onto the row it writes.
func userInsertArgs(user *User, groupIDs []string) []any {
	// Substitute an empty slice for nil so the Postgres TEXT[] column
	// stays at its default '{}' rather than NULL. The schema declares
	// the column NOT NULL DEFAULT '{}' (migration 000052); passing a
	// raw nil here triggers "violates not-null constraint" on insert.
	recoveryCodes := user.MFARecoveryCodes
	if recoveryCodes == nil {
		recoveryCodes = []string{}
	}
	return []any{
		user.ID,
		user.Email,
		user.PasswordHash,
		groupIDs,
		user.Active,
		user.MFAEnabled,
		user.MFASecret,
		user.MFAPendingSecret,
		user.MFAPendingSecretExpiresAt,
		recoveryCodes,
		user.PasswordResetToken,
		user.PasswordResetExpiry,
		user.FailedLoginAttempts,
		user.LockedUntil,
		user.PasswordHistory,
		user.CreatedAt,
		user.UpdatedAt,
		user.LastLoginAt,
	}
}

// Scanner interface for both Row and Rows.
type Scanner interface {
	Scan(dest ...any) error
}

func nullTimePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

// scanUser scans a user from a row that selected userSelectColumns.
func (s *PostgresStore) scanUser(scanner Scanner) (*User, error) {
	var user User
	var groupIDs []string
	var passwordHistory []string
	var recoveryCodes []string
	var resetExpiry, lockedUntil, lastLoginAt, mfaPendingExpiry, deactivatedAt sql.NullTime
	var mfaSecret, mfaPendingSecret, resetToken sql.NullString

	err := scanner.Scan(
		&user.ID,
		&user.Email,
		&user.PasswordHash,
		&groupIDs,
		&user.Active,
		&user.MFAEnabled,
		&mfaSecret,
		&mfaPendingSecret,
		&mfaPendingExpiry,
		&recoveryCodes,
		&resetToken,
		&resetExpiry,
		&user.FailedLoginAttempts,
		&lockedUntil,
		&passwordHistory,
		&user.CreatedAt,
		&user.UpdatedAt,
		&lastLoginAt,
		&deactivatedAt,
		&user.PasswordVersion,
	)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, pgx.ErrNoRows
		}
		return nil, fmt.Errorf("failed to scan user: %w", err)
	}

	user.GroupIDs = groupIDs
	user.PasswordHistory = passwordHistory
	user.MFARecoveryCodes = recoveryCodes

	// Handle nullable strings
	if mfaSecret.Valid {
		user.MFASecret = mfaSecret.String
	}
	if mfaPendingSecret.Valid {
		user.MFAPendingSecret = mfaPendingSecret.String
	}
	if resetToken.Valid {
		user.PasswordResetToken = resetToken.String
	}

	// Handle nullable timestamps
	user.MFAPendingSecretExpiresAt = nullTimePtr(mfaPendingExpiry)
	user.PasswordResetExpiry = nullTimePtr(resetExpiry)
	user.LockedUntil = nullTimePtr(lockedUntil)
	user.LastLoginAt = nullTimePtr(lastLoginAt)
	user.DeactivatedAt = nullTimePtr(deactivatedAt)

	return &user, nil
}

// Ping checks the database connection health.
func (s *PostgresStore) Ping(ctx context.Context) error {
	return s.db.Ping(ctx)
}
