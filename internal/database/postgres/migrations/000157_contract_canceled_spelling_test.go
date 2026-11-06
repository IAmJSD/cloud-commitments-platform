//go:build integration

package migrations_test

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The legacy spellings are built by concatenation so the US-locale misspell
// linter does not flag them; they are the values under test.
const (
	contract157     = 157
	legacyStatus157 = "cancel" + "led"
	legacyActor157  = "cancel" + "led_by"
	canceled157     = "canceled"
)

// legacyInCheck157 matches the legacy spelling's entry in a status CHECK
// definition as pg_get_constraintdef renders it (the cast follows the column
// type: text or character varying).
var legacyInCheck157 = regexp.MustCompile(`'` + legacyStatus157 + `'::[a-z ]+, `)

const insertExecution157 = `
	INSERT INTO purchase_executions (status, scheduled_date, ` + legacyActor157 + `, updated_at)
	VALUES ($1, NOW(), $2, $3) RETURNING execution_id::text`

func seedExecution157(ctx context.Context, t *testing.T, pool *pgxpool.Pool, status string, legacyBy *string, updatedAt time.Time) string {
	t.Helper()
	var id string
	require.NoError(t, pool.QueryRow(ctx, insertExecution157, status, legacyBy, updatedAt).Scan(&id))
	return id
}

func insertExchange157(ctx context.Context, pool *pgxpool.Pool, status string, updatedAt time.Time) (string, error) {
	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO ri_exchange_history
		    (account_id, region, source_ri_ids, source_instance_type, source_count,
		     target_offering_id, target_instance_type, target_count, status, updated_at)
		VALUES ('123456789012', 'us-east-1', ARRAY['ri-1'], 'm5.large', 1,
		        'offer-1', 'm5.xlarge', 1, $1, $2)
		RETURNING id::text`, status, updatedAt).Scan(&id)
	return id, err
}

func statusCheckDef157(ctx context.Context, t *testing.T, pool *pgxpool.Pool, constraint string) string {
	t.Helper()
	var def string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`, constraint).Scan(&def))
	return def
}

func updatedAt157(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table, id string) time.Time {
	t.Helper()
	idCol := map[string]string{"purchase_executions": "execution_id", "ri_exchange_history": "id"}[table]
	var ts time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT updated_at FROM `+table+` WHERE `+idCol+` = $1`, id).Scan(&ts))
	return ts
}

// readActor157 reads the actor the way every projection does.
func readActor157(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id string) (status string, actor *string) {
	t.Helper()
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, COALESCE(canceled_by, `+legacyActor157+`) FROM purchase_executions WHERE execution_id = $1`, id,
	).Scan(&status, &actor))
	return status, actor
}

func requireCheckViolation157(t *testing.T, err error, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "23514", pgErr.Code)
	assert.Equal(t, constraint, pgErr.ConstraintName)
}

func s157(v string) *string { return &v }

// TestMigration_000157_ContractCanceledSpelling locks down the contract half of
// the 000089 rename (issue #47): legacy status rows are normalized without
// restamping updated_at (the plan-health and retention clock), both status
// CHECKs lose exactly the legacy spelling, the down restores the widened
// CHECKs exactly, and the write statements of the build before this release
// (which still write the legacy actor column) keep working against the
// narrowed schema.
func TestMigration_000157_ContractCanceledSpelling(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { require.NoError(t, container.Cleanup(context.Background())) })
	pool, path := container.DB.Pool(), getMigrationsPath()
	prev := previousMigrationVersion(t, contract157)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))
	peBefore := statusCheckDef157(ctx, t, pool, "purchase_executions_status_check")
	riBefore := statusCheckDef157(ctx, t, pool, "ri_exchange_history_status_check")
	require.Len(t, legacyInCheck157.FindAllString(peBefore, -1), 1, peBefore)
	require.Len(t, legacyInCheck157.FindAllString(riBefore, -1), 1, riBefore)

	old := time.Now().UTC().AddDate(0, 0, -90).Truncate(time.Microsecond)
	legacyExec := seedExecution157(ctx, t, pool, legacyStatus157, s157("legacy@example.com"), old)
	failedExec := seedExecution157(ctx, t, pool, "failed", nil, old)
	legacyExchange, err := insertExchange157(ctx, pool, legacyStatus157, old)
	require.NoError(t, err)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, contract157))

	// Data: the legacy status is normalized and the legacy actor column is
	// left alone (this release still reads it); other rows are untouched.
	status, actor := readActor157(ctx, t, pool, legacyExec)
	assert.Equal(t, canceled157, status)
	require.NotNil(t, actor)
	assert.Equal(t, "legacy@example.com", *actor)
	status, _ = readActor157(ctx, t, pool, failedExec)
	assert.Equal(t, "failed", status)
	var exchangeStatus string
	require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM ri_exchange_history WHERE id = $1`, legacyExchange).Scan(&exchangeStatus))
	assert.Equal(t, canceled157, exchangeStatus)

	// updated_at is the plan-health and retention clock: a spelling change
	// must not make 90-day-old rows look fresh. Each table has its own
	// updated_at trigger, so each is asserted on its own.
	assert.True(t, old.Equal(updatedAt157(ctx, t, pool, "purchase_executions", legacyExec)),
		"purchase_executions.updated_at must survive the normalization")
	assert.True(t, old.Equal(updatedAt157(ctx, t, pool, "ri_exchange_history", legacyExchange)),
		"ri_exchange_history.updated_at must survive the normalization")
	var enabled int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_trigger
		WHERE tgname IN ('update_purchase_executions_updated_at', 'ri_exchange_updated_at')
		  AND tgenabled = 'O'`).Scan(&enabled))
	assert.Equal(t, 2, enabled, "both updated_at triggers must be enabled again")

	// Schema: each CHECK is its pre-migration definition minus the legacy
	// spelling, nothing else, and rejects the legacy spelling.
	assert.Equal(t, legacyInCheck157.ReplaceAllString(peBefore, ""),
		statusCheckDef157(ctx, t, pool, "purchase_executions_status_check"))
	assert.Equal(t, legacyInCheck157.ReplaceAllString(riBefore, ""),
		statusCheckDef157(ctx, t, pool, "ri_exchange_history_status_check"))
	_, err = pool.Exec(ctx, insertExecution157, legacyStatus157, nil, old)
	requireCheckViolation157(t, err, "purchase_executions_status_check")
	_, err = insertExchange157(ctx, pool, legacyStatus157, old)
	requireCheckViolation157(t, err, "ri_exchange_history_status_check")

	requireOldBuildWritesStillWork157(ctx, t, pool)

	// Down: the widened CHECKs come back byte for byte.
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))
	assert.Equal(t, peBefore, statusCheckDef157(ctx, t, pool, "purchase_executions_status_check"))
	assert.Equal(t, riBefore, statusCheckDef157(ctx, t, pool, "ri_exchange_history_status_check"))
	seedExecution157(ctx, t, pool, legacyStatus157, nil, old)
	_, err = insertExchange157(ctx, pool, legacyStatus157, old)
	require.NoError(t, err, "down must accept the legacy spelling again")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, contract157), "up must re-apply cleanly after down")
}

// Write statements copied from origin/main (internal/config/store_postgres.go
// at the time of #1253): the build that is live when this migration lands
// writes the legacy actor column, so none of them may hit the narrowed CHECKs
// and the actor must read back through COALESCE over both columns.
//
//nolint:misspell // the SQL deliberately names the legacy British column under test
const (
	oldSaveExecution157 = `
		INSERT INTO purchase_executions (
			plan_id, execution_id, status, step_number, scheduled_date,
			notification_sent, approval_token_hash, recommendations,
			total_upfront_cost, estimated_savings, completed_at, error, expires_at,
			cloud_account_id, source, approved_by, cancelled_by, capacity_percent,
			created_by_user_id, retry_execution_id, retry_attempt_n,
			approval_token_expires_at,
			executed_by_user_id, executed_at, pre_approval_skip_reason,
			idempotency_key, scheduled_execution_at, no_submission, created_by_system
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29)
		ON CONFLICT (execution_id) DO UPDATE SET
			status = $3,
			notification_sent = $6,
			approval_token_hash = $7,
			recommendations = $8,
			total_upfront_cost = $9,
			estimated_savings = $10,
			completed_at = $11,
			error = $12,
			expires_at = $13,
			cloud_account_id = $14,
			source = $15,
			approved_by = $16,
			cancelled_by = $17,
			capacity_percent = $18,
			approval_token_expires_at = $22,
			executed_by_user_id = $23,
			executed_at = $24,
			pre_approval_skip_reason = $25,
			scheduled_execution_at = $27,
			no_submission = CASE WHEN $3 = 'failed'
				THEN purchase_executions.no_submission OR $28
				ELSE false END,
			updated_at = NOW()`
	oldSetCancelledBy157 = `UPDATE purchase_executions SET canceled_by = $2, cancelled_by = $2, updated_at = NOW()
	       WHERE execution_id = $1`
	oldTransition157 = `
		UPDATE purchase_executions
		SET status = $2, updated_at = NOW(),
		    transitioned_by = $4, transitioned_at = NOW(),
		    canceled_by = COALESCE((SELECT email FROM users WHERE id = $5), canceled_by),
		    cancelled_by = COALESCE((SELECT email FROM users WHERE id = $5), cancelled_by)
		WHERE execution_id = $1 AND status = ANY($3)`
	oldCancelAllExchanges157 = `
		UPDATE ri_exchange_history
		SET status = $1, updated_at = NOW()
		WHERE status = 'pending'`
)

func requireOldBuildWritesStillWork157(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	now := time.Now().UTC()
	var execID string
	require.NoError(t, pool.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&execID))
	save := func(status, actor string) error {
		_, err := pool.Exec(ctx, oldSaveExecution157,
			nil, execID, status, 1, now, nil, nil, []byte(`[]`), 0, 0, nil, "", nil,
			nil, "web", nil, actor, 100, nil, nil, 0, nil, nil, nil, nil, nil, nil, false, false)
		return err
	}

	// SavePurchaseExecutionTx: insert, then the ON CONFLICT update, both with
	// the canonical status and the legacy actor column only.
	require.NoError(t, save("pending", ""), "old upsert insert path")
	require.NoError(t, save(canceled157, "old-save@example.com"), "old upsert update path")
	status, actor := readActor157(ctx, t, pool, execID)
	assert.Equal(t, canceled157, status)
	require.NotNil(t, actor, "an actor written to the legacy column must read back")
	assert.Equal(t, "old-save@example.com", *actor)

	// SetCancelledBy.
	ct, err := pool.Exec(ctx, oldSetCancelledBy157, execID, "set-by@example.com")
	require.NoError(t, err)
	require.EqualValues(t, 1, ct.RowsAffected())
	_, actor = readActor157(ctx, t, pool, execID)
	require.NotNil(t, actor)
	assert.Equal(t, "set-by@example.com", *actor)

	// transitionExecutionStatus dual write.
	var userID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, group_ids) VALUES ('transition@example.com', 'x', ARRAY[gen_random_uuid()]) RETURNING id::text`).Scan(&userID))
	transitionID := seedExecution157(ctx, t, pool, "pending", nil, now)
	ct, err = pool.Exec(ctx, oldTransition157, transitionID, canceled157, []string{"pending"}, userID, userID)
	require.NoError(t, err)
	require.EqualValues(t, 1, ct.RowsAffected())
	status, actor = readActor157(ctx, t, pool, transitionID)
	assert.Equal(t, canceled157, status)
	require.NotNil(t, actor)
	assert.Equal(t, "transition@example.com", *actor)

	// CancelAllPendingExchanges.
	exchangeID, err := insertExchange157(ctx, pool, "pending", now)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, oldCancelAllExchanges157, canceled157)
	require.NoError(t, err)
	var exchangeStatus string
	require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM ri_exchange_history WHERE id = $1`, exchangeID).Scan(&exchangeStatus))
	assert.Equal(t, canceled157, exchangeStatus)
}
