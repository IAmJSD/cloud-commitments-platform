//go:build integration

package auth

// Issue #313: round-trips every users column through the store on a real,
// migrated schema. Rows are written and read back with SQL that names its
// columns explicitly (in an order unrelated to userInsertColumns and
// userSelectColumns), so the check does not lean on the constants under
// test. A swap of two same-typed columns in either list, in scanUser or in
// userInsertArgs puts a value in the wrong place and fails here.

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// utcPtr normalizes a nullable timestamp read back from Postgres.
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// normalizeUserTimes puts every timestamp in UTC at Postgres's microsecond
// precision so a struct comparison checks values, not locations.
func normalizeUserTimes(u *User) *User {
	c := *u
	c.CreatedAt = c.CreatedAt.UTC().Truncate(time.Microsecond)
	c.UpdatedAt = c.UpdatedAt.UTC().Truncate(time.Microsecond)
	c.MFAPendingSecretExpiresAt = utcPtr(c.MFAPendingSecretExpiresAt)
	c.PasswordResetExpiry = utcPtr(c.PasswordResetExpiry)
	c.LockedUntil = utcPtr(c.LockedUntil)
	c.LastLoginAt = utcPtr(c.LastLoginAt)
	c.DeactivatedAt = utcPtr(c.DeactivatedAt)
	return &c
}

// insertUserByName writes u with an INSERT that names every column itself.
func insertUserByName(t *testing.T, db *database.Connection, u *User) {
	t.Helper()
	_, err := db.Exec(context.Background(), `
		INSERT INTO users (
			password_version, deactivated_at, last_login_at, updated_at, created_at,
			password_history, locked_until, failed_login_attempts,
			password_reset_expiry, password_reset_token, mfa_recovery_codes,
			mfa_pending_secret_expires_at, mfa_pending_secret, mfa_secret,
			mfa_enabled, active, group_ids, password_hash, email, id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)`,
		u.PasswordVersion, u.DeactivatedAt, u.LastLoginAt, u.UpdatedAt, u.CreatedAt,
		u.PasswordHistory, u.LockedUntil, u.FailedLoginAttempts,
		u.PasswordResetExpiry, u.PasswordResetToken, u.MFARecoveryCodes,
		u.MFAPendingSecretExpiresAt, u.MFAPendingSecret, u.MFASecret,
		u.MFAEnabled, u.Active, u.GroupIDs, u.PasswordHash, u.Email, u.ID,
	)
	require.NoError(t, err)
}

// readUserByName reads the row for id with a SELECT that names every column
// itself, independent of userSelectColumns and scanUser.
func readUserByName(t *testing.T, db *database.Connection, id string) *User {
	t.Helper()
	var u User
	var mfaSecret, mfaPending, resetToken *string
	err := db.QueryRow(context.Background(), `
		SELECT password_version, deactivated_at, last_login_at, updated_at, created_at,
		       password_history, locked_until, failed_login_attempts,
		       password_reset_expiry, password_reset_token, mfa_recovery_codes,
		       mfa_pending_secret_expires_at, mfa_pending_secret, mfa_secret,
		       mfa_enabled, active, group_ids::text[], password_hash, email, id::text
		FROM users WHERE id = $1`, id).Scan(
		&u.PasswordVersion, &u.DeactivatedAt, &u.LastLoginAt, &u.UpdatedAt, &u.CreatedAt,
		&u.PasswordHistory, &u.LockedUntil, &u.FailedLoginAttempts,
		&u.PasswordResetExpiry, &resetToken, &u.MFARecoveryCodes,
		&u.MFAPendingSecretExpiresAt, &mfaPending, &mfaSecret,
		&u.MFAEnabled, &u.Active, &u.GroupIDs, &u.PasswordHash, &u.Email, &u.ID,
	)
	require.NoError(t, err)
	for dst, src := range map[*string]*string{&u.MFASecret: mfaSecret, &u.MFAPendingSecret: mfaPending, &u.PasswordResetToken: resetToken} {
		if src != nil {
			*dst = *src
		}
	}
	return &u
}

func TestIntegration_UserColumns_ScanUserRoundTripsEveryColumn(t *testing.T) {
	db := setupAuthTestDB(t)
	store := NewPostgresStore(db)
	ctx := context.Background()
	want := distinctColumnsUser()
	insertUserByName(t, db, want)

	byID, err := store.GetUserByID(ctx, want.ID)
	require.NoError(t, err)
	assert.Equal(t, want, normalizeUserTimes(byID), "GetUserByID")

	byEmail, err := store.GetUserByEmail(ctx, want.Email)
	require.NoError(t, err)
	assert.Equal(t, want, normalizeUserTimes(byEmail), "GetUserByEmail")

	byToken, err := store.GetUserByResetToken(ctx, want.PasswordResetToken)
	require.NoError(t, err)
	assert.Equal(t, want, normalizeUserTimes(byToken), "GetUserByResetToken")

	users, err := store.ListUsers(ctx)
	require.NoError(t, err)
	require.Len(t, users, 1)
	assert.Equal(t, want, normalizeUserTimes(&users[0]), "ListUsers")
}

// insertedByStore is what a store INSERT of u must leave in the row:
// every written field as given, deactivated_at and password_version at
// their column defaults.
func insertedByStore(u *User) *User {
	c := *u
	c.DeactivatedAt = nil
	c.PasswordVersion = 0
	return normalizeUserTimes(&c)
}

func TestIntegration_UserColumns_CreateUserWritesEveryColumn(t *testing.T) {
	db := setupAuthTestDB(t)
	store := NewPostgresStore(db)
	user := distinctColumnsUser()
	require.NoError(t, store.CreateUser(context.Background(), user))

	assert.Equal(t, insertedByStore(user), normalizeUserTimes(readUserByName(t, db, user.ID)))
}

func TestIntegration_UserColumns_CreateAdminIfNoneWritesEveryColumn(t *testing.T) {
	db := setupAuthTestDB(t)
	store := NewPostgresStore(db)
	user := distinctColumnsUser() // already carries the Administrators group
	created, err := store.CreateAdminIfNone(context.Background(), user)
	require.NoError(t, err)
	require.True(t, created)

	assert.Equal(t, insertedByStore(user), normalizeUserTimes(readUserByName(t, db, user.ID)))
}
