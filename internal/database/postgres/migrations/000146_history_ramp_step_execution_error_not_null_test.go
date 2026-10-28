//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	m146Version = 146
	m146UpFile  = "000146_history_ramp_step_execution_error_not_null.up.sql"
)

// m146InsertHistory seeds a purchase_history row with the given ramp_step
// (nil seeds NULL, the legacy shape). Rows are read back by purchase_id.
func m146InsertHistory(ctx context.Context, t *testing.T, pool *pgxpool.Pool, purchaseID string, rampStep *int) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO purchase_history (
			account_id, purchase_id, timestamp, provider, service, region,
			resource_type, term, payment, ramp_step
		) VALUES ('test-account', $1, NOW(), 'aws', 'ec2', 'us-east-1',
		          'm5.large', 1, 'all-upfront', $2)`, purchaseID, rampStep)
	require.NoError(t, err)
}

// m146InsertExecution seeds a purchase_executions row with the given error
// (nil seeds NULL) and an updated_at an hour in the past, and returns its
// execution_id and the stored updated_at.
func m146InsertExecution(ctx context.Context, t *testing.T, pool *pgxpool.Pool, errText *string) (string, time.Time) {
	t.Helper()
	var id string
	var updatedAt time.Time
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO purchase_executions (status, scheduled_date, error, updated_at)
		VALUES ('failed', NOW(), $1, NOW() - INTERVAL '1 hour')
		RETURNING execution_id::text, updated_at`, errText).Scan(&id, &updatedAt))
	return id, updatedAt
}

func m146RampStep(ctx context.Context, t *testing.T, pool *pgxpool.Pool, purchaseID string) *int {
	t.Helper()
	var v *int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT ramp_step FROM purchase_history WHERE purchase_id = $1`, purchaseID).Scan(&v))
	return v
}

func m146Execution(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id string) (*string, time.Time) {
	t.Helper()
	var errText *string
	var updatedAt time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT error, updated_at FROM purchase_executions WHERE execution_id = $1`, id).Scan(&errText, &updatedAt))
	return errText, updatedAt
}

// m146Column reports a column's nullability and default expression.
func m146Column(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table, column string) (nullable bool, def *string) {
	t.Helper()
	var isNullable string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT is_nullable, column_default FROM information_schema.columns
		WHERE table_name = $1 AND column_name = $2`, table, column).Scan(&isNullable, &def))
	return isNullable == "YES", def
}

func m146RequireNotNullViolation(t *testing.T, err error, msg string) {
	t.Helper()
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "%s: want a PostgreSQL error, got %v", msg, err)
	assert.Equal(t, "23502", pgErr.Code, "%s: want not_null_violation", msg)
}

func m146Ptr[T any](v T) *T { return &v }

// TestMigration146_BackfillsAndEnforcesNotNull locks down migration 000146
// (issue #1484) on a real PostgreSQL:
//   - legacy NULL ramp_step / error rows are backfilled to 0 / empty string and every
//     existing row survives with its other values intact;
//   - the backfill does not stamp purchase_executions.updated_at, and the
//     updated_at trigger is live again afterwards;
//   - both columns are NOT NULL with a default, so an insert that omits them
//     gets 0 / empty string and an explicit NULL is rejected;
//   - GetAllPurchaseHistory lists the formerly-NULL row (before the migration,
//     the NULL fails the scan of the whole listing);
//   - the up file is idempotent and the down file makes both columns nullable
//     again with no default.
//
// Fail-before: at the previous version the NULL rows stay NULL,
// GetAllPurchaseHistory fails to scan, and an explicit NULL insert succeeds.
func TestMigration146_BackfillsAndEnforcesNotNull(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()
	migrationsPath := getMigrationsPath()
	prev := previousMigrationVersion(t, m146Version)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, prev))

	m146InsertHistory(ctx, t, pool, "ri-146-null", nil)
	m146InsertHistory(ctx, t, pool, "ri-146-step", m146Ptr(3))
	nullExec, nullExecUpdated := m146InsertExecution(ctx, t, pool, nil)
	errExec, _ := m146InsertExecution(ctx, t, pool, m146Ptr("boom"))

	store := config.NewPostgresStore(container.DB)
	_, err := store.GetAllPurchaseHistory(ctx, 100)
	require.Error(t, err, "precondition: a NULL ramp_step fails the history scan before 000146")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, m146Version))

	assert.Equal(t, m146Ptr(0), m146RampStep(ctx, t, pool, "ri-146-null"), "NULL ramp_step is backfilled to 0")
	assert.Equal(t, m146Ptr(3), m146RampStep(ctx, t, pool, "ri-146-step"), "a set ramp_step is untouched")
	gotErr, gotUpdated := m146Execution(ctx, t, pool, nullExec)
	assert.Equal(t, m146Ptr(""), gotErr, "NULL error is backfilled to ''")
	assert.True(t, gotUpdated.Equal(nullExecUpdated),
		"the backfill must not stamp updated_at: got %s, want %s", gotUpdated, nullExecUpdated)
	gotErr, _ = m146Execution(ctx, t, pool, errExec)
	assert.Equal(t, m146Ptr("boom"), gotErr, "a set error is untouched")

	history, err := store.GetAllPurchaseHistory(ctx, 100)
	require.NoError(t, err, "the formerly-NULL row no longer breaks the listing")
	assert.Len(t, history, 2)

	m146AssertConstraints(ctx, t, pool)
	m146AssertUpdatedAtTriggerLive(ctx, t, pool, nullExec, nullExecUpdated)

	up, err := os.ReadFile(filepath.Join(migrationsPath, m146UpFile))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(up))
	require.NoError(t, err, "the up file must be idempotent")
	m146AssertConstraints(ctx, t, pool)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, prev))
	nullable, def := m146Column(ctx, t, pool, "purchase_history", "ramp_step")
	assert.True(t, nullable, "down: ramp_step is nullable again")
	assert.Nil(t, def, "down: ramp_step has no default")
	nullable, def = m146Column(ctx, t, pool, "purchase_executions", "error")
	assert.True(t, nullable, "down: error is nullable again")
	assert.Nil(t, def, "down: error has no default")
	m146InsertHistory(ctx, t, pool, "ri-146-down", nil)
	m146InsertExecution(ctx, t, pool, nil)
	assert.Equal(t, m146Ptr(3), m146RampStep(ctx, t, pool, "ri-146-step"), "down keeps existing rows")
}

// m146AssertConstraints checks both columns are NOT NULL with the 0 / empty string
// default: an insert that omits them gets the default, an explicit NULL fails.
func m146AssertConstraints(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	nullable, def := m146Column(ctx, t, pool, "purchase_history", "ramp_step")
	assert.False(t, nullable, "ramp_step is NOT NULL")
	require.NotNil(t, def)
	assert.Equal(t, "0", *def)
	nullable, def = m146Column(ctx, t, pool, "purchase_executions", "error")
	assert.False(t, nullable, "error is NOT NULL")
	require.NotNil(t, def)
	assert.Equal(t, "''::text", *def)

	purchaseID := "ri-146-default-" + time.Now().Format("150405.000000000")
	_, err := pool.Exec(ctx, `
		INSERT INTO purchase_history (account_id, purchase_id, timestamp, provider, service, region, resource_type, term, payment)
		VALUES ('test-account', $1, NOW(), 'aws', 'ec2', 'us-east-1', 'm5.large', 1, 'all-upfront')`, purchaseID)
	require.NoError(t, err)
	assert.Equal(t, m146Ptr(0), m146RampStep(ctx, t, pool, purchaseID), "an omitted ramp_step defaults to 0")
	_, err = pool.Exec(ctx, `DELETE FROM purchase_history WHERE purchase_id = $1`, purchaseID)
	require.NoError(t, err)

	var execID, errText string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO purchase_executions (status, scheduled_date) VALUES ('failed', NOW())
		RETURNING execution_id::text, error`).Scan(&execID, &errText))
	assert.Equal(t, "", errText, "an omitted error defaults to ''")
	_, err = pool.Exec(ctx, `DELETE FROM purchase_executions WHERE execution_id = $1`, execID)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
		INSERT INTO purchase_history (account_id, purchase_id, timestamp, provider, service, region, resource_type, term, payment, ramp_step)
		VALUES ('test-account', 'ri-146-explicit-null', NOW(), 'aws', 'ec2', 'us-east-1', 'm5.large', 1, 'all-upfront', NULL)`)
	m146RequireNotNullViolation(t, err, "explicit NULL ramp_step")
	_, err = pool.Exec(ctx, `INSERT INTO purchase_executions (status, scheduled_date, error) VALUES ('failed', NOW(), NULL)`)
	m146RequireNotNullViolation(t, err, "explicit NULL error")
}

// m146AssertUpdatedAtTriggerLive checks the migration re-enabled
// update_purchase_executions_updated_at: an ordinary update stamps a fresh
// updated_at again.
func m146AssertUpdatedAtTriggerLive(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id string, before time.Time) {
	t.Helper()
	var enabled string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT tgenabled::text FROM pg_trigger
		WHERE tgrelid = 'purchase_executions'::regclass AND tgname = 'update_purchase_executions_updated_at'`).Scan(&enabled))
	assert.Equal(t, "O", enabled, "the updated_at trigger is enabled after the migration")

	_, err := pool.Exec(ctx, `UPDATE purchase_executions SET error = 'later' WHERE execution_id = $1`, id)
	require.NoError(t, err)
	_, after := m146Execution(ctx, t, pool, id)
	assert.True(t, after.After(before), "a real update stamps updated_at again")
}

// TestMigration146_ConcurrentNullWriter covers a writer still running an older
// build (or a hand-written INSERT) that has a NULL ramp_step / error insert in
// flight when the migration starts. The migration must wait for it, then
// backfill its row and succeed. Without the up-front table lock, the backfill
// UPDATE does not see the uncommitted row, SET NOT NULL waits for the writer,
// then finds the NULL and fails the whole migration.
func TestMigration146_ConcurrentNullWriter(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()
	migrationsPath := getMigrationsPath()
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, previousMigrationVersion(t, m146Version)))

	writer, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = writer.Rollback(ctx) }()
	_, err = writer.Exec(ctx, `
		INSERT INTO purchase_history (account_id, purchase_id, timestamp, provider, service, region, resource_type, term, payment, ramp_step)
		VALUES ('test-account', 'ri-146-race', NOW(), 'aws', 'ec2', 'us-east-1', 'm5.large', 1, 'all-upfront', NULL)`)
	require.NoError(t, err)
	var raceExec string
	require.NoError(t, writer.QueryRow(ctx, `
		INSERT INTO purchase_executions (status, scheduled_date, error) VALUES ('failed', NOW(), NULL)
		RETURNING execution_id::text`).Scan(&raceExec))

	done := make(chan error, 1)
	go func() { done <- migrations.MigrateToVersion(ctx, pool, migrationsPath, m146Version) }()

	// Wait until the migration is blocked on one of the two tables, so the
	// writer commits while the migration is mid-flight.
	require.Eventually(t, func() bool {
		var waiting int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_locks
			WHERE NOT granted
			  AND relation IN ('purchase_history'::regclass, 'purchase_executions'::regclass)`).Scan(&waiting); err != nil {
			return false
		}
		return waiting > 0
	}, time.Minute, 20*time.Millisecond, "the migration should wait on the in-flight writer")

	require.NoError(t, writer.Commit(ctx))
	require.NoError(t, <-done, "the migration must absorb a NULL committed while it waited")

	assert.Equal(t, m146Ptr(0), m146RampStep(ctx, t, pool, "ri-146-race"))
	gotErr, _ := m146Execution(ctx, t, pool, raceExec)
	assert.Equal(t, m146Ptr(""), gotErr)
	nullable, _ := m146Column(ctx, t, pool, "purchase_history", "ramp_step")
	assert.False(t, nullable)
	nullable, _ = m146Column(ctx, t, pool, "purchase_executions", "error")
	assert.False(t, nullable)
}
