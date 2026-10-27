package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// isDuplicateKeyError checks if the error is a PostgreSQL unique constraint violation (code 23505).
func isDuplicateKeyError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// isEmailDuplicateError returns true only when the violated constraint is the
// users_email_key (or users_email_unique) constraint, so we don't mistakenly
// surface "email already in use" for ID collisions or other uniqueness violations.
func isEmailDuplicateError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName == "users_email_key" ||
			pgErr.ConstraintName == "users_email_unique"
	}
	return false
}

// isLastAdminConstraintViolation reports whether err is the deferred trigger
// exception from migration 000065 (trg_min_one_admin). The trigger uses
// RAISE EXCEPTION with a message prefixed by "last_admin_constraint_violation"
// and PostgreSQL error code P0001 (raise_exception). Detecting by message
// prefix rather than code alone avoids false positives from other user-raised
// exceptions in the same codebase.
//
// The function also handles plain errors whose message contains the sentinel
// prefix, which allows unit tests to simulate the trigger violation without a
// live PostgreSQL connection.
func isLastAdminConstraintViolation(err error) bool {
	if err == nil {
		return false
	}
	const sentinel = "last_admin_constraint_violation"
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "P0001" && strings.HasPrefix(pgErr.Message, sentinel)
	}
	// Fallback for unit-test stubs that return a plain error with the sentinel
	// message (no pgconn available in mock-based tests).
	return strings.HasPrefix(err.Error(), sentinel)
}

// adminGroupContainsClause is the SQL predicate matching users who are members
// of the Administrators group. Authorization is group-membership-only after
// issue #907, so "is an admin" == "group_ids contains the Administrators group
// UUID". $1 must be bound to DefaultAdminGroupID.
const adminGroupContainsClause = `group_ids @> ARRAY[$1::uuid]`

// AdminExists checks if any active Administrators-group member exists. This is
// the group-membership replacement for the former role = 'admin' check.
func (s *PostgresStore) AdminExists(ctx context.Context) (bool, error) {
	query := `SELECT EXISTS(SELECT 1 FROM users WHERE ` + adminGroupContainsClause + ` AND active = true)`

	var exists bool
	err := s.db.QueryRow(ctx, query, DefaultAdminGroupID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("failed to check admin existence: %w", err)
	}

	return exists, nil
}

// CountGroupMembers returns the number of users whose group_ids contains
// groupID. Used to enforce last-administrator protection (issue #907).
func (s *PostgresStore) CountGroupMembers(ctx context.Context, groupID string) (int, error) {
	query := `SELECT COUNT(*) FROM users WHERE group_ids @> ARRAY[$1::uuid]`

	var count int
	if err := s.db.QueryRow(ctx, query, groupID).Scan(&count); err != nil {
		return 0, fmt.Errorf("failed to count group members: %w", err)
	}
	return count, nil
}

// adminInvariantAdvisoryLockKey is the transaction-scoped advisory lock key
// serializing writes that affect the "at least/at most the right number of
// admins" invariants. It MUST stay equal to the key used by
// check_min_one_admin() in migration 000065 so bootstrap inserts and
// admin-demoting commits serialize against each other as well as among
// themselves.
//
// Convention (issue #63): any transaction that may demote, deactivate or
// delete an Administrators-group member, or create the first admin, must
// acquire this lock as its first statement, before it locks any users row.
// Use withAdminInvariantLock. The deferred trigger takes the same key again at
// commit, which is then a no-op re-acquire. A write that locked a users row
// first and only reached this lock in the trigger would hold the row while
// waiting for the lock, and deadlock against a writer holding the lock while
// waiting for that row. Writes that cannot take admin standing away (login
// counters, MFA and password columns) do not take the lock; they only ever
// hold the row lock, so they cannot complete such a cycle.
const adminInvariantAdvisoryLockKey = 8059058058580001

// withAdminInvariantLock runs fn in a transaction whose first statement takes
// adminInvariantAdvisoryLockKey, then commits. The min-one-admin trigger runs
// at that commit, so its last_admin_constraint_violation comes back wrapped
// from here. op names the write in begin, lock and commit errors; errors from
// fn are returned unchanged. The lock auto-releases at COMMIT or ROLLBACK.
func (s *PostgresStore) withAdminInvariantLock(ctx context.Context, op string, fn func(tx pgx.Tx) error) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to %s: begin transaction: %w", op, err)
	}
	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil {
			_ = rbErr // no-op after Commit; Postgres logs real rollback failures
		}
	}()

	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", adminInvariantAdvisoryLockKey); err != nil {
		return fmt.Errorf("failed to %s: acquire admin invariant lock: %w", op, err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to %s: commit: %w", op, err)
	}
	return nil
}

// CreateAdminIfNone atomically inserts user as the first admin in the
// system. Returns (true, nil) when the insert succeeded; (false, nil)
// when an admin already existed (TOCTOU race — both callers passed
// AdminExists, only one wins the insert); (false, ErrEmailInUse) when
// the email collides with an existing (non-admin) user; (false, err)
// for any other failure.
//
// The conditional INSERT closes the bootstrap race without the
// users_one_admin partial unique index (dropped in migration 000050),
// but a single INSERT … WHERE NOT EXISTS statement is NOT race-free on
// its own: under READ COMMITTED each statement's snapshot is taken at
// statement start, so two concurrent calls can both see "no admin" and
// both insert (reproduced by
// TestIntegration_CreateAdminIfNone_ConcurrentBootstrapOnce). To close
// that window the insert runs in a transaction that first takes the
// transaction-scoped advisory lock shared with the min-one-admin
// trigger (migration 000065): the second caller blocks until the first
// commits, and its INSERT statement then takes a fresh snapshot that
// sees the committed admin, so the NOT EXISTS guard suppresses the
// duplicate. The lock auto-releases at COMMIT or ROLLBACK.
func (s *PostgresStore) CreateAdminIfNone(ctx context.Context, user *User) (bool, error) {
	if user.ID == "" {
		user.ID = uuid.New().String()
	}
	now := time.Now()
	user.CreatedAt = now
	user.UpdatedAt = now

	// The NOT EXISTS guard matches AdminExists's semantics exactly
	// (Administrators-group member AND active=true) so the fast-path check
	// and the atomic insert agree: a deployment with only inactive admins is
	// treated as "no admin" by both, and the insert proceeds. If they
	// disagreed, AdminExists could report false while this insert
	// failed silently on the WHERE clause, leaving the operator with
	// a recoverable-looking error and no admin.
	//
	// The bootstrap admin's group_ids is forced to include the Administrators
	// group regardless of the caller-supplied slice — the method's contract is
	// "create admin if none", and an admin row that did NOT carry the
	// Administrators group would not satisfy the membership predicate, silently
	// breaking that contract.
	query := `
		INSERT INTO users (
			id, email, password_hash, group_ids, active,
			mfa_enabled, mfa_secret, mfa_pending_secret, mfa_pending_secret_expires_at,
			mfa_recovery_codes, password_reset_token, password_reset_expiry,
			failed_login_attempts, locked_until, password_history,
			created_at, updated_at, last_login_at
		)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18
		WHERE NOT EXISTS (SELECT 1 FROM users WHERE group_ids @> ARRAY[$19::uuid] AND active = true)
	`

	// See CreateUser for the nil-slice substitution rationale. Force the
	// Administrators group onto the bootstrap admin so it satisfies the
	// membership predicate above (deduped to avoid a doubled entry if the
	// caller already supplied it).
	recoveryCodes := user.MFARecoveryCodes
	if recoveryCodes == nil {
		recoveryCodes = []string{}
	}
	groupIDs := user.GroupIDs
	if !containsGroup(groupIDs, DefaultAdminGroupID) {
		groupIDs = append(append([]string(nil), groupIDs...), DefaultAdminGroupID)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to begin admin bootstrap transaction: %w", err)
	}
	// Rollback is a no-op after a successful Commit; on any earlier return
	// it also releases the advisory lock. Matches the project convention
	// (e.g. internal/config/store_postgres.go) for deferred rollback.
	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil {
			_ = rbErr // rollback error intentionally discarded; Postgres logs rollback failures
		}
	}()

	// Serialize against concurrent bootstrap calls and against the
	// min-one-admin deferred trigger (see adminInvariantAdvisoryLockKey).
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", adminInvariantAdvisoryLockKey); err != nil {
		return false, fmt.Errorf("failed to acquire admin bootstrap lock: %w", err)
	}

	tag, err := tx.Exec(ctx, query,
		user.ID, user.Email, user.PasswordHash,
		groupIDs, user.Active, user.MFAEnabled, user.MFASecret,
		user.MFAPendingSecret, user.MFAPendingSecretExpiresAt, recoveryCodes,
		user.PasswordResetToken, user.PasswordResetExpiry,
		user.FailedLoginAttempts, user.LockedUntil, user.PasswordHistory,
		user.CreatedAt, user.UpdatedAt, user.LastLoginAt,
		DefaultAdminGroupID,
	)
	if err != nil {
		// Email uniqueness can still fire if the bootstrap caller reuses
		// an email already held by a non-admin user. Surface as the same
		// sentinel the regular create path uses so callers get a clean
		// "email already in use" instead of a raw DB error.
		if isEmailDuplicateError(err) {
			return false, ErrEmailInUse
		}
		return false, fmt.Errorf("failed to create admin: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("failed to commit admin bootstrap transaction: %w", err)
	}

	return tag.RowsAffected() > 0, nil
}
