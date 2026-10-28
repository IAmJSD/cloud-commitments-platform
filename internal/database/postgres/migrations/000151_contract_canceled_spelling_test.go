//go:build integration

package migrations_test

import (
	"context"
	"regexp"
	"sync"
	"sync/atomic"
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
	contract151     = 151
	legacyStatus151 = "cancel" + "led"
	legacyActor151  = "cancel" + "led_by"
)

// legacyInCheck151 matches the legacy spelling's entry in a status CHECK
// definition as pg_get_constraintdef renders it (the cast follows the column
// type: text or character varying).
var legacyInCheck151 = regexp.MustCompile(`'` + legacyStatus151 + `'::[a-z ]+, `)

const insertLegacyExecution151 = `
	INSERT INTO purchase_executions (status, scheduled_date, canceled_by, ` + legacyActor151 + `)
	VALUES ($1, NOW(), $2, $3) RETURNING execution_id::text`

func insertExecution151(ctx context.Context, t *testing.T, pool *pgxpool.Pool, status string, canceledBy, legacyBy *string) string {
	t.Helper()
	var id string
	require.NoError(t, pool.QueryRow(ctx, insertLegacyExecution151, status, canceledBy, legacyBy).Scan(&id))
	return id
}

func insertExchange151(ctx context.Context, pool *pgxpool.Pool, status string) (string, error) {
	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO ri_exchange_history
		    (account_id, region, source_ri_ids, source_instance_type, source_count,
		     target_offering_id, target_instance_type, target_count, status)
		VALUES ('123456789012', 'us-east-1', ARRAY['ri-1'], 'm5.large', 1,
		        'offer-1', 'm5.xlarge', 1, $1)
		RETURNING id::text`, status).Scan(&id)
	return id, err
}

func statusCheckDef151(ctx context.Context, t *testing.T, pool *pgxpool.Pool, constraint string) string {
	t.Helper()
	var def string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`, constraint).Scan(&def))
	return def
}

func actors151(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id string) (status string, canceledBy, legacyBy *string) {
	t.Helper()
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, canceled_by, `+legacyActor151+` FROM purchase_executions WHERE execution_id = $1`, id,
	).Scan(&status, &canceledBy, &legacyBy))
	return status, canceledBy, legacyBy
}

func requireCheckViolation151(t *testing.T, err error, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "23514", pgErr.Code)
	assert.Equal(t, constraint, pgErr.ConstraintName)
}

func s151(v string) *string { return &v }

// TestMigration_000151_ContractCanceledSpelling locks down the contract half of
// the 000089 rename (issue #47): legacy status rows are normalized, legacy-only
// actors are drained into canceled_by, both status CHECKs lose exactly the
// legacy spelling, writes from an older build that still sets the legacy actor
// column keep canceled_by authoritative, and the down restores the 000089 state.
func TestMigration_000151_ContractCanceledSpelling(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { require.NoError(t, container.Cleanup(context.Background())) })
	pool, path := container.DB.Pool(), getMigrationsPath()
	prev := previousMigrationVersion(t, contract151)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))
	peBefore := statusCheckDef151(ctx, t, pool, "purchase_executions_status_check")
	riBefore := statusCheckDef151(ctx, t, pool, "ri_exchange_history_status_check")
	require.Len(t, legacyInCheck151.FindAllString(peBefore, -1), 1, peBefore)
	require.Len(t, legacyInCheck151.FindAllString(riBefore, -1), 1, riBefore)

	legacyOnly := insertExecution151(ctx, t, pool, legacyStatus151, nil, s151("legacy@example.com"))
	// Already canonical status, actor only in the legacy column: only the
	// drain reaches it (the status UPDATE does not touch the row).
	drainOnly := insertExecution151(ctx, t, pool, canceled151, nil, s151("drain@example.com"))
	both := insertExecution151(ctx, t, pool, canceled151, s151("new@example.com"), s151("new@example.com"))
	conflicting := insertExecution151(ctx, t, pool, legacyStatus151, s151("kept@example.com"), s151("other@example.com"))
	failed := insertExecution151(ctx, t, pool, "failed", nil, nil)
	exchange, err := insertExchange151(ctx, pool, legacyStatus151)
	require.NoError(t, err)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, contract151))

	// Data: every legacy status normalized, every legacy-only actor drained,
	// canceled_by kept where both columns were set, other rows untouched.
	for id, want := range map[string]struct{ status, actor string }{
		legacyOnly:  {canceled151, "legacy@example.com"},
		drainOnly:   {canceled151, "drain@example.com"},
		both:        {canceled151, "new@example.com"},
		conflicting: {canceled151, "kept@example.com"},
	} {
		status, canceledBy, _ := actors151(ctx, t, pool, id)
		assert.Equal(t, want.status, status, id)
		require.NotNil(t, canceledBy, id)
		assert.Equal(t, want.actor, *canceledBy, id)
	}
	status, canceledBy, _ := actors151(ctx, t, pool, failed)
	assert.Equal(t, "failed", status)
	assert.Nil(t, canceledBy)
	var exchangeStatus string
	require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM ri_exchange_history WHERE id = $1`, exchange).Scan(&exchangeStatus))
	assert.Equal(t, canceled151, exchangeStatus)

	// Schema: each CHECK is its 000089 definition minus the legacy spelling,
	// nothing else, and rejects the legacy spelling.
	assert.Equal(t, legacyInCheck151.ReplaceAllString(peBefore, ""),
		statusCheckDef151(ctx, t, pool, "purchase_executions_status_check"))
	assert.Equal(t, legacyInCheck151.ReplaceAllString(riBefore, ""),
		statusCheckDef151(ctx, t, pool, "ri_exchange_history_status_check"))
	_, err = pool.Exec(ctx, insertLegacyExecution151, legacyStatus151, nil, nil)
	requireCheckViolation151(t, err, "purchase_executions_status_check")
	_, err = insertExchange151(ctx, pool, legacyStatus151)
	requireCheckViolation151(t, err, "ri_exchange_history_status_check")

	// An older build still writes the legacy actor column (its save path
	// writes only that column). canceled_by must end up with the value that
	// build reads, COALESCE(canceled_by, legacy), on insert and on update.
	lateInsert := insertExecution151(ctx, t, pool, canceled151, nil, s151("late@example.com"))
	_, canceledBy, _ = actors151(ctx, t, pool, lateInsert)
	require.NotNil(t, canceledBy, "a legacy-only insert must be mirrored into canceled_by")
	assert.Equal(t, "late@example.com", *canceledBy)
	_, err = pool.Exec(ctx, `UPDATE purchase_executions SET `+legacyActor151+` = 'late-update@example.com' WHERE execution_id = $1`, failed)
	require.NoError(t, err)
	_, canceledBy, _ = actors151(ctx, t, pool, failed)
	require.NotNil(t, canceledBy, "a legacy-only update must be mirrored into canceled_by")
	assert.Equal(t, "late-update@example.com", *canceledBy)
	_, err = pool.Exec(ctx, `UPDATE purchase_executions SET `+legacyActor151+` = 'stale@example.com' WHERE execution_id = $1`, both)
	require.NoError(t, err)
	_, canceledBy, _ = actors151(ctx, t, pool, both)
	assert.Equal(t, "new@example.com", *canceledBy, "a legacy write never overrides an actor already in canceled_by")

	// New code writes canceled_by only.
	newOnly := insertExecution151(ctx, t, pool, canceled151, s151("current@example.com"), nil)

	// Down: back to the 000089 CHECKs, trigger gone, legacy column backfilled.
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))
	assert.Equal(t, peBefore, statusCheckDef151(ctx, t, pool, "purchase_executions_status_check"))
	assert.Equal(t, riBefore, statusCheckDef151(ctx, t, pool, "ri_exchange_history_status_check"))
	insertExecution151(ctx, t, pool, legacyStatus151, nil, nil)
	_, err = insertExchange151(ctx, pool, legacyStatus151)
	require.NoError(t, err, "down must accept the legacy spelling again")
	_, _, legacyBy := actors151(ctx, t, pool, newOnly)
	require.NotNil(t, legacyBy, "down must backfill the legacy actor column")
	assert.Equal(t, "current@example.com", *legacyBy)
	afterDown := insertExecution151(ctx, t, pool, canceled151, nil, s151("x@example.com"))
	_, canceledBy, _ = actors151(ctx, t, pool, afterDown)
	assert.Nil(t, canceledBy, "down must drop the mirror trigger")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, contract151), "up must re-apply cleanly after down")
}

// canceled151 is the canonical spelling, kept local so this frozen
// migration test does not move if the config constant ever does.
const canceled151 = "canceled"

// TestMigration_000151_ConcurrentLegacyWritersKeepAttribution races writers
// shaped like an older build (legacy actor column only) against the migration.
// Whichever side of the migration a write lands on, canceled_by must hold its
// actor afterwards: rows committed before the migration are drained, rows
// written after it are mirrored by the trigger, and the table lock leaves no
// gap between the two.
func TestMigration_000151_ConcurrentLegacyWritersKeepAttribution(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { require.NoError(t, container.Cleanup(context.Background())) })
	pool, path := container.DB.Pool(), getMigrationsPath()
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, previousMigrationVersion(t, contract151)))

	var migrated atomic.Bool
	var afterMigration atomic.Int64
	stop := make(chan struct{})
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				done := migrated.Load()
				if _, err := pool.Exec(ctx, insertLegacyExecution151, canceled151, nil, "racer@example.com"); err != nil {
					errs <- err
					return
				}
				if done {
					afterMigration.Add(1)
				}
			}
		}()
	}

	time.Sleep(200 * time.Millisecond)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, contract151))
	migrated.Store(true)
	require.Eventually(t, func() bool { return afterMigration.Load() >= 20 }, time.Minute, 10*time.Millisecond)
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err, "a legacy writer must not fail across the migration")
	}

	var total, undrained int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE canceled_by IS NULL)
		FROM purchase_executions WHERE `+legacyActor151+` IS NOT NULL`).Scan(&total, &undrained))
	require.Positive(t, total)
	assert.Zero(t, undrained, "every legacy-only actor must be in canceled_by, of %d rows", total)
}
