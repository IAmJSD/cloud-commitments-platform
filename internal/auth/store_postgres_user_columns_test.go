package auth

// Issue #313: every user SELECT reads userSelectColumns and scanUser scans
// it positionally; both INSERTs write userInsertColumns from
// userInsertArgs. Several neighboring columns share a Go type (mfa_secret
// and mfa_pending_secret, the three []string columns, the timestamps), so a
// list and its scan or argument order can drift apart without any error.
// These tests key every value by column name, so a reordering on either side
// puts a value into the wrong field and fails. The integration variant in
// store_postgres_user_columns_db_test.go repeats the round trip on a real,
// migrated schema.

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// splitColumnList turns a SQL column list into trimmed column names.
func splitColumnList(list string) []string {
	parts := strings.Split(list, ",")
	names := make([]string, 0, len(parts))
	for _, p := range parts {
		names = append(names, strings.TrimSpace(p))
	}
	return names
}

// columnFixtureTimes returns seven distinct timestamps, one per timestamp
// column, so a swap between any two of them is visible.
func columnFixtureTimes() [7]time.Time {
	base := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	var ts [7]time.Time
	for i := range ts {
		ts[i] = base.Add(time.Duration(i+1) * time.Hour)
	}
	return ts
}

// distinctColumnsUser returns a user whose every field holds a value no other
// same-typed field holds. Active and MFAEnabled differ so a swap of the two
// booleans shows too.
func distinctColumnsUser() *User {
	ts := columnFixtureTimes()
	return &User{
		ID:                        "0c5e3a1d-6f0b-4b7e-9a51-3d2f6c7b8e90",
		Email:                     "columns-313@example.com",
		PasswordHash:              "hash-current",
		GroupIDs:                  []string{DefaultAdminGroupID, DefaultPurchaserGroupID},
		Active:                    false,
		MFAEnabled:                true,
		MFASecret:                 "mfa-secret-enrolled",
		MFAPendingSecret:          "mfa-secret-pending",
		MFAPendingSecretExpiresAt: &ts[0],
		MFARecoveryCodes:          []string{"recovery-a", "recovery-b"},
		PasswordResetToken:        "reset-token-hash",
		PasswordResetExpiry:       &ts[1],
		FailedLoginAttempts:       3,
		LockedUntil:               &ts[2],
		PasswordHistory:           []string{"hash-old-1", "hash-old-2", "hash-old-3"},
		CreatedAt:                 ts[3],
		UpdatedAt:                 ts[4],
		LastLoginAt:               &ts[5],
		DeactivatedAt:             &ts[6],
		PasswordVersion:           9,
	}
}

// userRowByColumn maps each users column to the value a row for u would
// carry, keyed by name rather than position.
func userRowByColumn(u *User) map[string]any {
	return map[string]any{
		"id":                            u.ID,
		"email":                         u.Email,
		"password_hash":                 u.PasswordHash,
		"group_ids":                     u.GroupIDs,
		"active":                        u.Active,
		"mfa_enabled":                   u.MFAEnabled,
		"mfa_secret":                    u.MFASecret,
		"mfa_pending_secret":            u.MFAPendingSecret,
		"mfa_pending_secret_expires_at": *u.MFAPendingSecretExpiresAt,
		"mfa_recovery_codes":            u.MFARecoveryCodes,
		"password_reset_token":          u.PasswordResetToken,
		"password_reset_expiry":         *u.PasswordResetExpiry,
		"failed_login_attempts":         u.FailedLoginAttempts,
		"locked_until":                  *u.LockedUntil,
		"password_history":              u.PasswordHistory,
		"created_at":                    u.CreatedAt,
		"updated_at":                    u.UpdatedAt,
		"last_login_at":                 *u.LastLoginAt,
		"deactivated_at":                *u.DeactivatedAt,
		"password_version":              u.PasswordVersion,
	}
}

// userInsertArgsByColumn maps each insert column to the Exec argument the
// store must bind for u. created_at and updated_at are set by the store
// itself, so they are matched as "a time.Time inside the call window".
func userInsertArgsByColumn(u *User, after time.Time) map[string]any {
	recent := pgxmock.ArgumentFunc(func(v any) bool {
		ts, ok := v.(time.Time)
		return ok && !ts.Before(after) && !ts.After(time.Now())
	})
	return map[string]any{
		"id":                            u.ID,
		"email":                         u.Email,
		"password_hash":                 u.PasswordHash,
		"group_ids":                     u.GroupIDs,
		"active":                        u.Active,
		"mfa_enabled":                   u.MFAEnabled,
		"mfa_secret":                    u.MFASecret,
		"mfa_pending_secret":            u.MFAPendingSecret,
		"mfa_pending_secret_expires_at": u.MFAPendingSecretExpiresAt,
		"mfa_recovery_codes":            u.MFARecoveryCodes,
		"password_reset_token":          u.PasswordResetToken,
		"password_reset_expiry":         u.PasswordResetExpiry,
		"failed_login_attempts":         u.FailedLoginAttempts,
		"locked_until":                  u.LockedUntil,
		"password_history":              u.PasswordHistory,
		"created_at":                    recent,
		"updated_at":                    recent,
		"last_login_at":                 u.LastLoginAt,
	}
}

// valuesInListOrder looks every column of list up in byColumn and returns the
// values in the list's order. It fails the test on an unknown or duplicated
// column, or when byColumn has a column the list leaves out.
func valuesInListOrder(t *testing.T, list string, byColumn map[string]any) []any {
	t.Helper()
	names := splitColumnList(list)
	seen := make(map[string]bool, len(names))
	values := make([]any, 0, len(names))
	for _, name := range names {
		require.False(t, seen[name], "column %q listed twice", name)
		seen[name] = true
		v, ok := byColumn[name]
		require.True(t, ok, "column %q has no fixture value", name)
		values = append(values, v)
	}
	require.Len(t, names, len(byColumn), "column list and fixture disagree on the column set")
	return values
}

func TestUserSelectColumns_ScanUserReadsEveryColumnIntoItsField(t *testing.T) {
	want := distinctColumnsUser()
	names := splitColumnList(userSelectColumns)
	row := valuesInListOrder(t, userSelectColumns, userRowByColumn(want))
	// Every query must embed the shared list verbatim.
	selectRe := regexp.QuoteMeta("SELECT " + userSelectColumns + "\n\t\tFROM users")

	getters := []struct {
		name string
		args []any
		get  func(*PostgresStore) (*User, error)
	}{
		{"GetUserByID", []any{want.ID}, func(s *PostgresStore) (*User, error) {
			return s.GetUserByID(context.Background(), want.ID)
		}},
		{"GetUserByEmail", []any{want.Email}, func(s *PostgresStore) (*User, error) {
			return s.GetUserByEmail(context.Background(), want.Email)
		}},
		{"GetUserByResetToken", []any{want.PasswordResetToken}, func(s *PostgresStore) (*User, error) {
			return s.GetUserByResetToken(context.Background(), want.PasswordResetToken)
		}},
		{"ListUsers", nil, func(s *PostgresStore) (*User, error) {
			users, err := s.ListUsers(context.Background())
			if err != nil || len(users) != 1 {
				return nil, err
			}
			return &users[0], nil
		}},
	}
	for _, tc := range getters {
		t.Run(tc.name, func(t *testing.T) {
			mock := newAuthPgxMock(t)
			mock.ExpectQuery(selectRe).WithArgs(tc.args...).
				WillReturnRows(pgxmock.NewRows(names).AddRow(row...))

			got, err := tc.get(NewPostgresStore(mock))
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

func TestUserInsertColumns_CreateUserBindsEveryColumnFromItsField(t *testing.T) {
	mock := newAuthPgxMock(t)
	user := distinctColumnsUser()
	before := time.Now()
	args := valuesInListOrder(t, userInsertColumns, userInsertArgsByColumn(user, before))

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO users (" + userInsertColumns + ")")).
		WithArgs(args...).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	require.NoError(t, NewPostgresStore(mock).CreateUser(context.Background(), user))
}

func TestUserInsertColumns_CreateAdminIfNoneBindsEveryColumnFromItsField(t *testing.T) {
	mock := newAuthPgxMock(t)
	user := distinctColumnsUser() // already carries the Administrators group
	before := time.Now()
	args := valuesInListOrder(t, userInsertColumns, userInsertArgsByColumn(user, before))
	args = append(args, DefaultAdminGroupID) // $19, the NOT EXISTS binding

	expectBootstrapTxPrefix(mock)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO users (" + userInsertColumns + ")")).
		WithArgs(args...).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	created, err := NewPostgresStore(mock).CreateAdminIfNone(context.Background(), user)
	require.NoError(t, err)
	assert.True(t, created)
}

// The select list is the insert list plus the two columns the inserts leave
// to their defaults; pin that so the two cannot drift into different orders.
func TestUserSelectColumns_ExtendInsertColumns(t *testing.T) {
	insert := splitColumnList(userInsertColumns)
	sel := splitColumnList(userSelectColumns)
	require.Equal(t, insert, sel[:len(insert)])
	assert.Equal(t, []string{"deactivated_at", "password_version"}, sel[len(insert):])
}
