package auth

// store_postgres_admin_lock_test.go - pgxmock tests pinning that every
// PostgresStore write able to lower the active-admin count runs in a
// transaction whose first statement takes adminInvariantAdvisoryLockKey
// (issue #63). The lock-order and concurrency behavior itself is proven on a
// real PostgreSQL in store_postgres_admin_lock_db_test.go.

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	updateUserAdminFieldsSQLRegex = `(?s)UPDATE users SET email = \$2, group_ids = \$3, active = \$4, ` +
		`deactivated_at = \$5, updated_at = NOW\(\)\s+` +
		`WHERE id = \$1 AND email = \$6 AND group_ids = \$7 AND active = \$8\s+RETURNING updated_at`

	deleteUserSQLRegex = `DELETE FROM users WHERE id = \$1`
)

const lockedWriteUserID = "22222222-3333-4444-8555-666666666666"

// writeOutcome selects what the mocked write statement returns.
type writeOutcome int

const (
	writeOK writeOutcome = iota
	writeNoRow
	writeFails
)

// adminLockedWrite describes one admin-affecting store write: how to mock its
// statement for an outcome, how to call it, and the error a missing row gives.
type adminLockedWrite struct {
	name        string
	expect      func(mock pgxmock.PgxPoolIface, outcome writeOutcome)
	call        func(ctx context.Context, store *PostgresStore) error
	assertNoRow func(t *testing.T, err error)
}

func expectLockedExec(mock pgxmock.PgxPoolIface, sqlRegex, verb string, args []any, outcome writeOutcome) {
	e := mock.ExpectExec(sqlRegex).WithArgs(args...)
	switch outcome {
	case writeOK:
		e.WillReturnResult(pgxmock.NewResult(verb, 1))
	case writeNoRow:
		e.WillReturnResult(pgxmock.NewResult(verb, 0))
	case writeFails:
		e.WillReturnError(errors.New("connection reset"))
	}
}

func anyArgs(n int) []any {
	args := make([]any, n)
	for i := range args {
		args[i] = pgxmock.AnyArg()
	}
	return args
}

func adminLockedWrites() []adminLockedWrite {
	assertUserNotFound := func(t *testing.T, err error) {
		require.Error(t, err)
		assert.Contains(t, err.Error(), "user not found: "+lockedWriteUserID)
	}
	return []adminLockedWrite{
		{
			name: "UpdateUserAdminFields",
			expect: func(mock pgxmock.PgxPoolIface, outcome writeOutcome) {
				e := mock.ExpectQuery(updateUserAdminFieldsSQLRegex).
					WithArgs(lockedWriteUserID, "new@example.com", []string{DefaultPurchaserGroupID}, false,
						pgxmock.AnyArg(), "old@example.com", []string{DefaultAdminGroupID}, true)
				switch outcome {
				case writeOK:
					e.WillReturnRows(mock.NewRows([]string{"updated_at"}).AddRow(time.Unix(1700000000, 0)))
				case writeNoRow:
					e.WillReturnRows(mock.NewRows([]string{"updated_at"}))
				case writeFails:
					e.WillReturnError(errors.New("connection reset"))
				}
			},
			call: func(ctx context.Context, store *PostgresStore) error {
				user := &User{ID: lockedWriteUserID, Email: "new@example.com", GroupIDs: []string{DefaultPurchaserGroupID}}
				return store.UpdateUserAdminFields(ctx, user, "old@example.com", []string{DefaultAdminGroupID}, true)
			},
			assertNoRow: func(t *testing.T, err error) { assert.ErrorIs(t, err, ErrUserChanged) },
		},
		{
			name: "DeleteUser",
			expect: func(mock pgxmock.PgxPoolIface, outcome writeOutcome) {
				expectLockedExec(mock, deleteUserSQLRegex, "DELETE", []any{lockedWriteUserID}, outcome)
			},
			call: func(ctx context.Context, store *PostgresStore) error {
				return store.DeleteUser(ctx, lockedWriteUserID)
			},
			assertNoRow: assertUserNotFound,
		},
	}
}

// expectAdminLock registers the transaction open and the advisory lock that
// must precede every admin-affecting write. pgxmock matches in order, so a
// write issued before the lock fails the test.
func expectAdminLock(mock pgxmock.PgxPoolIface) {
	mock.ExpectBegin()
	mock.ExpectExec(advisoryLockSQLRegex).
		WithArgs(adminInvariantAdvisoryLockKey).
		WillReturnResult(pgxmock.NewResult("SELECT", 1))
}

func TestPGXMock_AdminWrites_TakeLockBeforeWriteAndCommit(t *testing.T) {
	for _, w := range adminLockedWrites() {
		t.Run(w.name, func(t *testing.T) {
			mock := newAuthPgxMock(t)
			expectAdminLock(mock)
			w.expect(mock, writeOK)
			mock.ExpectCommit()

			require.NoError(t, w.call(context.Background(), NewPostgresStore(mock)))
		})
	}
}

func TestPGXMock_AdminWrites_BeginError(t *testing.T) {
	for _, w := range adminLockedWrites() {
		t.Run(w.name, func(t *testing.T) {
			mock := newAuthPgxMock(t)
			mock.ExpectBegin().WillReturnError(errors.New("pool exhausted"))

			err := w.call(context.Background(), NewPostgresStore(mock))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "begin transaction")
		})
	}
}

func TestPGXMock_AdminWrites_LockErrorSkipsWrite(t *testing.T) {
	for _, w := range adminLockedWrites() {
		t.Run(w.name, func(t *testing.T) {
			mock := newAuthPgxMock(t)
			mock.ExpectBegin()
			mock.ExpectExec(advisoryLockSQLRegex).
				WithArgs(adminInvariantAdvisoryLockKey).
				WillReturnError(errors.New("canceling statement due to lock timeout"))
			mock.ExpectRollback()

			err := w.call(context.Background(), NewPostgresStore(mock))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "acquire admin invariant lock")
		})
	}
}

func TestPGXMock_AdminWrites_NoRowRollsBack(t *testing.T) {
	for _, w := range adminLockedWrites() {
		t.Run(w.name, func(t *testing.T) {
			mock := newAuthPgxMock(t)
			expectAdminLock(mock)
			w.expect(mock, writeNoRow)
			mock.ExpectRollback()

			w.assertNoRow(t, w.call(context.Background(), NewPostgresStore(mock)))
		})
	}
}

func TestPGXMock_AdminWrites_WriteErrorRollsBack(t *testing.T) {
	for _, w := range adminLockedWrites() {
		t.Run(w.name, func(t *testing.T) {
			mock := newAuthPgxMock(t)
			expectAdminLock(mock)
			w.expect(mock, writeFails)
			mock.ExpectRollback()

			err := w.call(context.Background(), NewPostgresStore(mock))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "connection reset")
		})
	}
}

// TestPGXMock_AdminWrites_CommitTriggerViolationStaysDetectable covers the
// deferred min-one-admin trigger, which now raises at the explicit COMMIT. The
// service maps it to ErrLastAdmin through isLastAdminConstraintViolation, so
// the PgError must survive the wrapping.
func TestPGXMock_AdminWrites_CommitTriggerViolationStaysDetectable(t *testing.T) {
	for _, w := range adminLockedWrites() {
		t.Run(w.name, func(t *testing.T) {
			mock := newAuthPgxMock(t)
			expectAdminLock(mock)
			w.expect(mock, writeOK)
			mock.ExpectCommit().WillReturnError(&pgconn.PgError{
				Code:    "P0001",
				Message: "last_admin_constraint_violation: at least one active member of the Administrators group must remain",
			})
			mock.ExpectRollback()

			err := w.call(context.Background(), NewPostgresStore(mock))
			require.Error(t, err)
			assert.True(t, isLastAdminConstraintViolation(err), "lost the trigger error: %v", err)
			assert.Contains(t, err.Error(), "commit")
		})
	}
}

// TestStore_HasNoWholeRowUserWriter pins issue #583: the whole-row
// UpdateUser(ctx, *User) is gone from the store, so adminLockedWrites above
// lists every store write that can lower the active-admin count. A new caller
// has to pick a column-scoped writer, and cannot bring back a stale whole-row
// save that overwrites MFA, password or membership changes made since its
// read.
func TestStore_HasNoWholeRowUserWriter(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeFor[StoreInterface](),
		reflect.TypeFor[*PostgresStore](),
	} {
		_, found := typ.MethodByName("UpdateUser")
		assert.False(t, found, "%s still has the whole-row UpdateUser", typ)
	}
}

func TestPGXMock_UpdateUserAdminFields_ReturnsStoredUpdatedAt(t *testing.T) {
	mock := newAuthPgxMock(t)
	stored := time.Unix(1700000000, 0)
	expectAdminLock(mock)
	mock.ExpectQuery(updateUserAdminFieldsSQLRegex).
		WithArgs(anyArgs(8)...).
		WillReturnRows(mock.NewRows([]string{"updated_at"}).AddRow(stored))
	mock.ExpectCommit()

	user := &User{ID: lockedWriteUserID}
	require.NoError(t, NewPostgresStore(mock).UpdateUserAdminFields(context.Background(), user, "", nil, true))
	assert.True(t, user.UpdatedAt.Equal(stored))
}
