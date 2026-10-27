//go:build integration

package config

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
)

// Deterministic lock tests for SaveAccountServiceOverrideAuditedTx (issue
// #1493). Each parks a competing writer mid-transaction, waits until the
// audited save is blocked on a lock, commits the writer, and checks the
// event's before is the row the save actually replaced.

type overrideAuditStoreFixture struct {
	store   *PostgresStore
	account string
}

func newOverrideAuditStoreFixture(ctx context.Context, t *testing.T) *overrideAuditStoreFixture {
	t.Helper()
	store := NewPostgresStore(testhelpers.RequireMigratedDatabase(ctx, t, getTestMigrationsPath()).DB)
	acct := &CloudAccount{Name: "audit lock fixture", Provider: "aws", ExternalID: "123456789012", Enabled: true}
	require.NoError(t, store.CreateCloudAccount(ctx, acct))
	return &overrideAuditStoreFixture{store: store, account: acct.ID}
}

func (f *overrideAuditStoreFixture) hold(ctx context.Context, t *testing.T) pgx.Tx {
	t.Helper()
	tx, err := f.store.db.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			t.Logf("writer rollback: %v", rbErr)
		}
	})
	return tx
}

// saveInBackground runs an audited save of term=3 for aws/rds.
func (f *overrideAuditStoreFixture) saveInBackground(ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() {
		done <- f.store.WithTx(ctx, func(tx pgx.Tx) error {
			o := &AccountServiceOverride{AccountID: f.account, Provider: "aws", Service: "rds", Term: intPtrAudit(3)}
			return f.store.SaveAccountServiceOverrideAuditedTx(ctx, tx, o, AuditContext{Actor: AuditActor{Identity: "admin@example.com"}})
		})
	}()
	return done
}

// lastEventBefore returns the newest upsert event's before (nil for NULL).
func (f *overrideAuditStoreFixture) lastEventBefore(ctx context.Context, t *testing.T) map[string]any {
	t.Helper()
	var raw []byte
	require.NoError(t, f.store.db.QueryRow(ctx,
		`SELECT before FROM config_audit_events WHERE event_type = $1 ORDER BY seq DESC LIMIT 1`, AuditOverrideUpsert).Scan(&raw))
	if raw == nil {
		return nil
	}
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func intPtrAudit(v int) *int { return &v }

// TestOverrideAudit_SaveQueuesBehindDelete: a delete removes the row while the
// save is starting. The save must wait for the row lock and then record a
// create (before NULL); reading the row before the delete committed would
// record the deleted row as replaced, so the trail would delete it twice.
func TestOverrideAudit_SaveQueuesBehindDelete(t *testing.T) {
	ctx := context.Background()
	f := newOverrideAuditStoreFixture(ctx, t)
	require.NoError(t, f.store.SaveAccountServiceOverride(ctx, &AccountServiceOverride{AccountID: f.account, Provider: "aws", Service: "rds", Term: intPtrAudit(1)}))

	writer := f.hold(ctx, t)
	_, err := writer.Exec(ctx, `DELETE FROM account_service_overrides WHERE account_id = $1`, f.account)
	require.NoError(t, err)
	done := f.saveInBackground(ctx)
	testhelpers.WaitForLockWait(ctx, t, f.store.db, "%account_service_overrides%")
	require.NoError(t, writer.Commit(ctx))
	require.NoError(t, testhelpers.Await(t, done))

	assert.Nil(t, f.lastEventBefore(ctx, t), "the save created the override anew")
}

// TestOverrideAudit_SaveQueuesBehindFirstCreate: another audited save holds
// the override's advisory lock and has inserted the row. A row lock cannot
// help (the row is not visible yet), so only the advisory lock makes this
// save wait and then record that row as its before.
func TestOverrideAudit_SaveQueuesBehindFirstCreate(t *testing.T) {
	ctx := context.Background()
	f := newOverrideAuditStoreFixture(ctx, t)

	writer := f.hold(ctx, t)
	_, err := writer.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))",
		accountServiceOverrideLockPrefix+f.account+"/aws/rds")
	require.NoError(t, err)
	require.NoError(t, saveAccountServiceOverride(ctx, writer, &AccountServiceOverride{AccountID: f.account, Provider: "aws", Service: "rds", Term: intPtrAudit(1)}))
	done := f.saveInBackground(ctx)
	testhelpers.WaitForLockWait(ctx, t, f.store.db, "")
	require.NoError(t, writer.Commit(ctx))
	require.NoError(t, testhelpers.Await(t, done))

	before := f.lastEventBefore(ctx, t)
	require.NotNil(t, before, "the save replaced the first create's row")
	assert.Equal(t, 1.0, before["term"])
}

// TestOverrideAudit_AfterIsTheStoredRow: after is read back from the write,
// so it shows what the DECIMAL(5,2) column holds, not the value sent.
func TestOverrideAudit_AfterIsTheStoredRow(t *testing.T) {
	ctx := context.Background()
	f := newOverrideAuditStoreFixture(ctx, t)
	coverage := 33.333
	o := &AccountServiceOverride{AccountID: f.account, Provider: "aws", Service: "rds", Coverage: &coverage}
	require.NoError(t, f.store.WithTx(ctx, func(tx pgx.Tx) error {
		return f.store.SaveAccountServiceOverrideAuditedTx(ctx, tx, o, AuditContext{Actor: AuditActor{Identity: "admin@example.com"}})
	}))
	var raw []byte
	require.NoError(t, f.store.db.QueryRow(ctx, `SELECT after FROM config_audit_events`).Scan(&raw))
	var after map[string]any
	require.NoError(t, json.Unmarshal(raw, &after))
	assert.Equal(t, 33.33, after["coverage"])
	assert.Equal(t, 33.33, *o.Coverage, "the caller's override is updated to the stored row")
}
