package config

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var overrideAuditCols = []string{
	"id", "account_id", "provider", "service",
	"enabled", "term", "payment", "coverage", "ramp_schedule",
	"include_engines", "exclude_engines", "include_regions", "exclude_regions",
	"include_types", "exclude_types", "created_at", "updated_at",
}

const auditTestAccount = "11111111-1111-1111-1111-111111111111"

func overrideAuditRow(term int) *pgxmock.Rows {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	return pgxmock.NewRows(overrideAuditCols).AddRow(
		"o-1", auditTestAccount, "aws", "rds",
		nil, &term, nil, nil, nil,
		nil, nil, []string{"us-east-1"}, nil, nil, nil, at, at,
	)
}

var testAuditCtx = AuditContext{Actor: AuditActor{Identity: "admin@example.com"}}

// runAudited runs fn in a pgxmock transaction (BEGIN ... COMMIT expected by
// the caller).
func runAudited(t *testing.T, mock pgxmock.PgxPoolIface, fn func(tx pgx.Tx) error) error {
	t.Helper()
	return storeWith(mock).WithTx(context.Background(), fn)
}

// expectOverrideLockedRead queues the advisory lock on the override's key and
// the FOR UPDATE read, in that order.
func expectOverrideLockedRead(mock pgxmock.PgxPoolIface, existing *pgxmock.Rows) {
	mock.ExpectExec("pg_advisory_xact_lock").WithArgs(accountServiceOverrideLockPrefix + auditTestAccount + "/aws/rds").
		WillReturnResult(pgxmock.NewResult("SELECT", 1))
	mock.ExpectQuery(`FROM account_service_overrides\s+WHERE account_id = \$1 AND provider = \$2 AND service = \$3 FOR UPDATE`).
		WithArgs(auditTestAccount, "aws", "rds").WillReturnRows(existing)
}

// TestPGXMock_SaveOverrideAudited_RecordsBeforeAndAfter: lock, locked read,
// upsert read back, then the event with both snapshots, in one transaction.
func TestPGXMock_SaveOverrideAudited_RecordsBeforeAndAfter(t *testing.T) {
	mock := newMock(t)
	mock.ExpectBegin()
	expectOverrideLockedRead(mock, overrideAuditRow(1))
	mock.ExpectQuery(`INSERT INTO account_service_overrides .* RETURNING`).WithArgs(anyArgsCfg(17)...).
		WillReturnRows(overrideAuditRow(3))
	mock.ExpectQuery("INSERT INTO config_audit_events").WithArgs(anyArgsCfg(12)...).
		WillReturnRows(pgxmock.NewRows([]string{"created_at"}).AddRow(time.Now()))
	mock.ExpectCommit()

	o := &AccountServiceOverride{ID: "fresh-id", AccountID: auditTestAccount, Provider: "aws", Service: "rds", Term: intPtrUnit(3)}
	require.NoError(t, runAudited(t, mock, func(tx pgx.Tx) error {
		return (&PostgresStore{db: mock}).SaveAccountServiceOverrideAuditedTx(context.Background(), tx, o, testAuditCtx)
	}))
	assert.Equal(t, "o-1", o.ID, "the caller sees the stored row's ID")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestPGXMock_SaveOverrideAudited_NoChangeNoEvent: a save that leaves every
// setting as it was records no event.
func TestPGXMock_SaveOverrideAudited_NoChangeNoEvent(t *testing.T) {
	mock := newMock(t)
	mock.ExpectBegin()
	expectOverrideLockedRead(mock, overrideAuditRow(1))
	mock.ExpectQuery(`INSERT INTO account_service_overrides .* RETURNING`).WithArgs(anyArgsCfg(17)...).
		WillReturnRows(overrideAuditRow(1))
	// No INSERT INTO config_audit_events.
	mock.ExpectCommit()

	o := &AccountServiceOverride{AccountID: auditTestAccount, Provider: "aws", Service: "rds", Term: intPtrUnit(1)}
	require.NoError(t, runAudited(t, mock, func(tx pgx.Tx) error {
		return (&PostgresStore{db: mock}).SaveAccountServiceOverrideAuditedTx(context.Background(), tx, o, testAuditCtx)
	}))
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestPGXMock_DeleteOverrideAudited: the deleted row is the event's before; a
// delete that finds nothing records nothing.
func TestPGXMock_DeleteOverrideAudited(t *testing.T) {
	t.Run("row deleted", func(t *testing.T) {
		mock := newMock(t)
		mock.ExpectBegin()
		mock.ExpectQuery(`DELETE FROM account_service_overrides .* RETURNING`).WithArgs(auditTestAccount, "aws", "rds").
			WillReturnRows(overrideAuditRow(1))
		mock.ExpectQuery("INSERT INTO config_audit_events").
			WithArgs(pgxmock.AnyArg(), AuditOverrideDelete, pgxmock.AnyArg(), "admin@example.com", pgxmock.AnyArg(), pgxmock.AnyArg(),
				pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), nil, nil).
			WillReturnRows(pgxmock.NewRows([]string{"created_at"}).AddRow(time.Now()))
		mock.ExpectCommit()
		require.NoError(t, runAudited(t, mock, func(tx pgx.Tx) error {
			return (&PostgresStore{db: mock}).DeleteAccountServiceOverrideAuditedTx(context.Background(), tx, auditTestAccount, "aws", "rds", testAuditCtx)
		}))
		assert.NoError(t, mock.ExpectationsWereMet())
	})
	t.Run("nothing to delete", func(t *testing.T) {
		mock := newMock(t)
		mock.ExpectBegin()
		mock.ExpectQuery(`DELETE FROM account_service_overrides .* RETURNING`).WithArgs(auditTestAccount, "aws", "rds").
			WillReturnRows(pgxmock.NewRows(overrideAuditCols))
		mock.ExpectCommit()
		require.NoError(t, runAudited(t, mock, func(tx pgx.Tx) error {
			return (&PostgresStore{db: mock}).DeleteAccountServiceOverrideAuditedTx(context.Background(), tx, auditTestAccount, "aws", "rds", testAuditCtx)
		}))
		assert.NoError(t, mock.ExpectationsWereMet())
	})
}

// TestPGXMock_SaveConfigAuditEvent_RequiresActor: an event with no actor is
// refused before it reaches the database, and the transaction rolls back.
func TestPGXMock_SaveConfigAuditEvent_RequiresActor(t *testing.T) {
	mock := newMock(t)
	mock.ExpectBegin()
	mock.ExpectRollback()
	err := runAudited(t, mock, func(tx pgx.Tx) error {
		return (&PostgresStore{db: mock}).SaveConfigAuditEventTx(context.Background(), tx, &AuditEvent{EventType: AuditOverrideImport})
	})
	require.ErrorContains(t, err, "no actor")
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestOverrideSnapshotJSON(t *testing.T) {
	got, err := overrideSnapshotJSON(&AccountServiceOverride{
		ID: "o-1", AccountID: auditTestAccount, Provider: "aws", Service: "rds",
		Term: intPtrUnit(3), IncludeRegions: []string{"us-east-1"}, CreatedAt: time.Now(),
	})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(got, &m))
	assert.Equal(t, 3.0, m["term"])
	assert.Equal(t, []any{"us-east-1"}, m["include_regions"])
	assert.Contains(t, m, "enabled", "an unset setting is recorded as null, not omitted")
	assert.NotContains(t, m, "id")
	assert.NotContains(t, m, "created_at")

	none, err := overrideSnapshotJSON(nil)
	require.NoError(t, err)
	assert.Nil(t, none)
}

func intPtrUnit(v int) *int { return &v }
