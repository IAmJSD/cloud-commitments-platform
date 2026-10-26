//go:build integration

package migrations_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const migration137 = 137

// Account identifiers ri_exchange_history.account_id must hold once exchanges
// are persisted for every cloud (issue #144).
const (
	exchAWSID        = "123456789012"                         // the only shape 000009 allowed
	exchAzureSub     = "3f2504e0-4f89-11d3-9a0c-0305e82c3301" // 36 chars: over VARCHAR(20) and not 12 digits
	exchGCPProject   = "cudly-production-analytics-001"       // 30 chars, the documented GCP maximum
	exchGCPShortProj = "cudly-prod-0042"                      // fits VARCHAR(20); only the CHECK rejects it
)

// insertExchangeRow issues an INSERT carrying every NOT NULL column of
// ri_exchange_history that has no default, the same column set
// SaveRIExchangeRecord always writes.
func insertExchangeRow(ctx context.Context, pool *pgxpool.Pool, accountID string) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO ri_exchange_history (
			account_id, region, source_ri_ids, source_instance_type, source_count,
			target_offering_id, target_instance_type, target_count
		) VALUES ($1, 'eastus', ARRAY['src-1'], 'Standard_D2s_v3', 1, 'offer-1', 'Standard_D4s_v3', 1)`,
		accountID)
	return err
}

func requireSQLState(t *testing.T, err error, code, why string) {
	t.Helper()
	require.Error(t, err, why)
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "expected a Postgres error, got %v", err)
	assert.Equal(t, code, pgErr.Code, why)
}

// exchangeAccountIDWidth reads the column width from information_schema, an
// independent probe from the pg_attribute lookup the migration itself uses.
// It returns -1 for unbounded varchar.
func exchangeAccountIDWidth(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var width *int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT character_maximum_length FROM information_schema.columns
		 WHERE table_schema = current_schema()
		   AND table_name = 'ri_exchange_history' AND column_name = 'account_id'`).Scan(&width))
	if width == nil {
		return -1
	}
	return *width
}

// requireAcceptsEveryCloud asserts the post-fix contract by behavior: an AWS,
// Azure and GCP identifier each insert and round-trip untruncated.
func requireAcceptsEveryCloud(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, id := range []string{exchAWSID, exchAzureSub, exchGCPProject, exchGCPShortProj} {
		require.NoError(t, insertExchangeRow(ctx, pool, id), "account_id %q must be accepted", id)
		var n int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM ri_exchange_history WHERE account_id = $1`, id).Scan(&n))
		assert.Positive(t, n, "account_id %q must round-trip untruncated", id)
	}
}

func readMigration137(t *testing.T, direction string) string {
	t.Helper()
	sql, err := os.ReadFile(filepath.Join(getMigrationsPath(),
		"000137_ri_exchange_history_account_id_multicloud."+direction+".sql"))
	require.NoError(t, err)
	return string(sql)
}

func startMigrationDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { _ = container.Cleanup(context.Background()) })
	return ctx, container.DB.Pool()
}

// TestMigration137_RIExchangeHistoryAccountIDMultiCloud is the
// fail-before/pass-after test for issue #144: before 000137 an Azure
// subscription ID is rejected by the width (22001) and a GCP project ID that
// fits the width is still rejected by the CHECK (23514); after it, every cloud's
// identifier is stored.
func TestMigration137_RIExchangeHistoryAccountIDMultiCloud(t *testing.T) {
	ctx, pool := startMigrationDB(t)
	path := getMigrationsPath()

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, previousMigrationVersion(t, migration137)))
	require.Equal(t, 20, exchangeAccountIDWidth(ctx, t, pool))
	require.NoError(t, insertExchangeRow(ctx, pool, exchAWSID), "an AWS account ID always fit")
	requireSQLState(t, insertExchangeRow(ctx, pool, exchAzureSub), "22001",
		"pre-fix: an Azure subscription GUID overflows VARCHAR(20)")
	requireSQLState(t, insertExchangeRow(ctx, pool, exchGCPShortProj), "23514",
		"pre-fix: a GCP project ID that fits the width is rejected by the AWS-only CHECK")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, migration137))
	assert.Equal(t, 255, exchangeAccountIDWidth(ctx, t, pool))
	requireAcceptsEveryCloud(ctx, t, pool)

	_, err := pool.Exec(ctx, readMigration137(t, "up"))
	require.NoError(t, err, "re-running the up migration must be a no-op")
	assert.Equal(t, 255, exchangeAccountIDWidth(ctx, t, pool))
}

// TestMigration137_RepairsPartialStates runs the up SQL against databases where
// only one barrier, or an unexpected variant of one, is present. Widening
// alone is not a fix (the CHECK still rejects Azure at any width), so each case
// must end with every cloud's identifier accepted.
func TestMigration137_RepairsPartialStates(t *testing.T) {
	ctx, pool := startMigrationDB(t)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, getMigrationsPath(),
		previousMigrationVersion(t, migration137)))
	up := readMigration137(t, "up")

	for _, tc := range []struct {
		name      string
		setup     string
		wantWidth int
	}{
		{"widened but still checked",
			`ALTER TABLE ri_exchange_history ALTER COLUMN account_id TYPE VARCHAR(255)`, 255},
		{"check dropped but still narrow",
			`ALTER TABLE ri_exchange_history DROP CONSTRAINT ri_exchange_history_account_id_check`, 255},
		{"check under another name",
			`ALTER TABLE ri_exchange_history RENAME CONSTRAINT ri_exchange_history_account_id_check TO restored_acct_shape`, 255},
		{"unbounded varchar is left wider",
			`ALTER TABLE ri_exchange_history ALTER COLUMN account_id TYPE VARCHAR`, -1},
		{"wider than 255 is not narrowed",
			`ALTER TABLE ri_exchange_history ALTER COLUMN account_id TYPE VARCHAR(300)`, 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetExchangeAccountIDTo000009(ctx, t, pool)
			_, err := pool.Exec(ctx, tc.setup)
			require.NoError(t, err)

			_, err = pool.Exec(ctx, up)
			require.NoError(t, err)
			assert.Equal(t, tc.wantWidth, exchangeAccountIDWidth(ctx, t, pool))
			requireAcceptsEveryCloud(ctx, t, pool)
		})
	}
}

// resetExchangeAccountIDTo000009 puts the column back to exactly what 000009
// created, so each partial-state case starts from the same schema.
func resetExchangeAccountIDTo000009(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		DELETE FROM ri_exchange_history;
		ALTER TABLE ri_exchange_history
			DROP CONSTRAINT IF EXISTS ri_exchange_history_account_id_check,
			DROP CONSTRAINT IF EXISTS restored_acct_shape,
			ALTER COLUMN account_id TYPE VARCHAR(20),
			ADD CONSTRAINT ri_exchange_history_account_id_check CHECK (account_id ~ '^\d{12}$');`)
	require.NoError(t, err)
}

// TestMigration137_ConcurrentRunsSerialize races two runs of the up SQL (the
// auto-heal or a manual replay running alongside a deploy). The second run
// starts while the first has done all its work but not committed. Without the
// table lock taken before the catalog reads, the second run sees the CHECK the
// first has already dropped, waits on the first run's lock, and then fails with
// "constraint does not exist" once it commits.
func TestMigration137_ConcurrentRunsSerialize(t *testing.T) {
	ctx, pool := startMigrationDB(t)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, getMigrationsPath(),
		previousMigrationVersion(t, migration137)))
	up := readMigration137(t, "up")

	for _, tc := range []struct{ name, setup string }{
		{"from the 000009 schema", ``},
		{"from widened but still checked",
			`ALTER TABLE ri_exchange_history ALTER COLUMN account_id TYPE VARCHAR(255)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetExchangeAccountIDTo000009(ctx, t, pool)
			if tc.setup != "" {
				_, err := pool.Exec(ctx, tc.setup)
				require.NoError(t, err)
			}

			first, err := pool.Begin(ctx)
			require.NoError(t, err)
			defer func() { _ = first.Rollback(context.Background()) }()
			_, err = first.Exec(ctx, up)
			require.NoError(t, err)

			second := make(chan error, 1)
			go func() {
				_, execErr := pool.Exec(ctx, up)
				second <- execErr
			}()

			require.Eventually(t, func() bool {
				var waiting int
				if qErr := pool.QueryRow(ctx, `
					SELECT count(*) FROM pg_locks
					 WHERE NOT granted AND relation = 'ri_exchange_history'::regclass`).Scan(&waiting); qErr != nil {
					return false
				}
				return waiting > 0
			}, 30*time.Second, 50*time.Millisecond, "the second run must block on the first")

			require.NoError(t, first.Commit(ctx))
			select {
			case execErr := <-second:
				require.NoError(t, execErr, "the second run must see the committed state and no-op")
			case <-time.After(30 * time.Second):
				t.Fatal("the second run did not finish after the first committed")
			}
			assert.Equal(t, 255, exchangeAccountIDWidth(ctx, t, pool))
			requireAcceptsEveryCloud(ctx, t, pool)
		})
	}
}

// TestMigration137_DownRestoresAWSConstraintWhenSafe covers the rollback's
// success path: with only AWS rows present it narrows the column and puts the
// CHECK back, and the up applies again afterwards.
func TestMigration137_DownRestoresAWSConstraintWhenSafe(t *testing.T) {
	ctx, pool := startMigrationDB(t)
	path := getMigrationsPath()

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, migration137))
	require.NoError(t, insertExchangeRow(ctx, pool, exchAWSID))

	// One step, so exactly 000137's down runs.
	require.NoError(t, migrations.RollbackMigrations(ctx, pool, path, 1))
	assert.Equal(t, 20, exchangeAccountIDWidth(ctx, t, pool), "the rollback must narrow the column")
	requireSQLState(t, insertExchangeRow(ctx, pool, exchGCPShortProj), "23514",
		"the rollback must restore the AWS-only CHECK")
	var stored string
	require.NoError(t, pool.QueryRow(ctx, `SELECT account_id FROM ri_exchange_history`).Scan(&stored))
	assert.Equal(t, exchAWSID, stored, "the lossless rollback must keep the AWS row")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, migration137),
		"the up must re-apply after a rollback")
	requireAcceptsEveryCloud(ctx, t, pool)
}

// TestMigration137_DownRefusesNonAWSRows: the rollback must fail loudly, with
// the rows intact, rather than truncate or drop exchange audit rows. The GCP
// case fits VARCHAR(20), so only the CHECK guard catches it.
func TestMigration137_DownRefusesNonAWSRows(t *testing.T) {
	for _, id := range []string{exchAzureSub, exchGCPShortProj} {
		t.Run(id, func(t *testing.T) {
			ctx, pool := startMigrationDB(t)
			path := getMigrationsPath()

			require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, migration137))
			require.NoError(t, insertExchangeRow(ctx, pool, id))

			err := migrations.RollbackMigrations(ctx, pool, path, 1)
			require.Error(t, err, "rolling back with a non-AWS row present must fail")
			// The migrate error embeds the file's SQL text, so match the
			// formatted row count: it appears only in the raised message.
			assert.Contains(t, err.Error(), "1 row(s) hold an account ID that is not a 12-digit AWS account ID")

			assert.Equal(t, 255, exchangeAccountIDWidth(ctx, t, pool), "the refused rollback must not narrow")
			var stored string
			require.NoError(t, pool.QueryRow(ctx, `SELECT account_id FROM ri_exchange_history`).Scan(&stored))
			assert.Equal(t, id, stored, "the refused rollback must leave the row untouched")
		})
	}
}
