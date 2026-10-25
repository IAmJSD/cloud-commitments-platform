//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigration_LadderTranchesSettledAt pins migration 000117 against a real
// PostgreSQL: completed rows that predate it are backfilled, the CHECK is
// enforced, deleting the account, config or execution of a legacy completed
// row still works (the ON DELETE SET NULL updates re-evaluate the CHECK), and
// the up statement is idempotent.
func TestMigration_LadderTranchesSettledAt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := getMigrationsPath()
	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, previousMigrationVersion(t, 117)))
	ids := seedLegacyCompletedTranches(ctx, t, pool)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 117))
	for name, id := range ids.tranches {
		var settled bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT settled_at IS NOT NULL FROM ladder_tranches WHERE id = $1`, id).Scan(&settled))
		assert.True(t, settled, "legacy completed row %s must be backfilled", name)
	}
	var validated bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT convalidated FROM pg_constraint WHERE conname = 'ladder_tranches_completed_has_settled_at'`).Scan(&validated))
	assert.True(t, validated)

	_, err := pool.Exec(ctx, `UPDATE ladder_tranches SET status_reason = 'x' WHERE id = $1`, ids.tranches["account"])
	require.NoError(t, err, "an unrelated update of a legacy completed row must succeed")
	_, err = pool.Exec(ctx, `DELETE FROM cloud_accounts WHERE id = $1`, ids.account)
	require.NoError(t, err, "deleting the account of a legacy completed tranche")
	_, err = pool.Exec(ctx, `DELETE FROM ladder_configs WHERE id = $1`, ids.config)
	require.NoError(t, err, "deleting the config of a legacy completed tranche")
	_, err = pool.Exec(ctx, `DELETE FROM purchase_executions WHERE execution_id = $1`, ids.execution)
	require.NoError(t, err, "deleting the execution of a legacy completed tranche")

	_, err = pool.Exec(ctx, `INSERT INTO ladder_tranches (layer_type, amount_usd_hr, term, payment_option, scheduled_date, status)
		VALUES ('compute-sp', 1, '1y', 'no_upfront', NOW(), 'completed')`)
	require.Error(t, err, "an unstamped completed row must be rejected")

	up, err := os.ReadFile(filepath.Join(path, "000117_ladder_tranches_settled_at.up.sql"))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(up))
	require.NoError(t, err, "the up statement must be idempotent")

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, previousMigrationVersion(t, 117)))
	var cols, cons int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'ladder_tranches' AND column_name = 'settled_at'`).Scan(&cols))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conname = 'ladder_tranches_completed_has_settled_at'`).Scan(&cons))
	assert.Equal(t, 0, cols+cons, "down drops the column and the constraint")
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 117), "up after down re-backfills completed rows")
}

type legacyTranches struct {
	account, config, execution string
	tranches                   map[string]string
}

func seedLegacyCompletedTranches(ctx context.Context, t *testing.T, pool *pgxpool.Pool) legacyTranches {
	t.Helper()
	l := legacyTranches{tranches: map[string]string{}}
	account := func(ext string) (acct, cfg string) {
		require.NoError(t, pool.QueryRow(ctx, `INSERT INTO cloud_accounts (name, enabled, provider, external_id, aws_is_org_root)
			VALUES ($1, true, 'aws', $1, false) RETURNING id`, ext).Scan(&acct))
		require.NoError(t, pool.QueryRow(ctx, `INSERT INTO ladder_configs
			(cloud_account_id, provider, mode, cadence, target_coverage, buffer_fraction, baseline_percentile,
			 lookback_days, buffer_utilization_threshold, max_actions_per_run, ramp_schedule)
			VALUES ($1, 'aws', 'auto_approve', 'daily', 80, 0.1, 5, 30, 50, 5, '{"steps":[{"after_days":0,"fraction":1}]}')
			RETURNING id`, acct).Scan(&cfg))
		return acct, cfg
	}
	tranche := func(name, cfg, exec string) {
		var cfgArg, execArg any
		if cfg != "" {
			cfgArg = cfg
		}
		if exec != "" {
			execArg = exec
		}
		var id string
		require.NoError(t, pool.QueryRow(ctx, `INSERT INTO ladder_tranches
			(config_id, execution_id, layer_type, amount_usd_hr, term, payment_option, scheduled_date, status)
			VALUES ($1, $2, 'compute-sp', 1, '1y', 'no_upfront', NOW(), 'completed') RETURNING id`, cfgArg, execArg).Scan(&id))
		l.tranches[name] = id
	}
	acct1, cfg1 := account("111111111111")
	_, cfg2 := account("222222222222")
	l.account, l.config = acct1, cfg2
	tranche("account", cfg1, "")
	tranche("config", cfg2, "")
	var plan string
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO purchase_plans (name) VALUES ('legacy') RETURNING id`).Scan(&plan))
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO purchase_executions (plan_id, scheduled_date) VALUES ($1, NOW()) RETURNING execution_id`, plan).Scan(&l.execution))
	tranche("execution", "", l.execution)
	return l
}
