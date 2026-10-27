//go:build integration

package migrations_test

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const rollupsUnknownVersion = 143

// snapshotRow143 is one savings_snapshots row; a nil cost is an unknown one.
type snapshotRow143 struct {
	account    string
	at         time.Time
	region     string
	usage      *float64
	commitment *float64
}

// rollup143 is what the two views report for one account.
type rollup143 struct {
	monthlyUsage, monthlyCommitment, providerCommitment *float64
}

func cost143(v float64) *float64 { return &v }

func insertSnapshots143(ctx context.Context, t *testing.T, pool *pgxpool.Pool, rows []snapshotRow143) {
	t.Helper()
	for _, r := range rows {
		_, err := pool.Exec(ctx, `INSERT INTO savings_snapshots
			(account_id, timestamp, provider, service, region, commitment_type,
			 total_commitment, total_usage, total_savings)
			VALUES ($1, $2, 'aws', 'ec2', $3, 'RI', $4, $5, 10)`,
			r.account, r.at, r.region, r.commitment, r.usage)
		require.NoError(t, err)
	}
}

func readRollup143(ctx context.Context, t *testing.T, pool *pgxpool.Pool, account string) rollup143 {
	t.Helper()
	var got rollup143
	require.NoError(t, pool.QueryRow(ctx, `SELECT total_usage::float8, total_commitment::float8
		FROM monthly_savings_summary WHERE account_id = $1`, account).
		Scan(&got.monthlyUsage, &got.monthlyCommitment), "monthly row for %s", account)
	require.NoError(t, pool.QueryRow(ctx, `SELECT total_commitment::float8
		FROM provider_savings_summary WHERE account_id = $1`, account).
		Scan(&got.providerCommitment), "provider row for %s", account)
	return got
}

// requireRollup143 checks every column against want, where nil means the view
// must report NULL (unknown) and a value means that exact known figure.
func requireRollup143(t *testing.T, label string, want *float64, got rollup143) {
	t.Helper()
	for name, v := range map[string]*float64{
		"monthly total_usage":       got.monthlyUsage,
		"monthly total_commitment":  got.monthlyCommitment,
		"provider total_commitment": got.providerCommitment,
	} {
		if want == nil {
			assert.Nil(t, v, "%s: %s must be unknown (NULL)", label, name)
			continue
		}
		if assert.NotNil(t, v, "%s: %s must be known", label, name) {
			assert.InDelta(t, *want, *v, 0.001, "%s: %s", label, name)
		}
	}
}

// TestMigration_SavingsRollupsPropagateUnknown locks down migration 000143
// (issue #151): monthly_savings_summary and provider_savings_summary report an
// unknown total_usage / total_commitment as NULL instead of summing or
// averaging past it, keep a known zero as 0, keep the H5 sum-then-average
// result for fully known data, and the down migration restores the previous
// view definitions exactly.
func TestMigration_SavingsRollupsPropagateUnknown(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { require.NoError(t, container.Cleanup(context.Background())) })
	pool, path := container.DB.Pool(), getMigrationsPath()
	prev := previousMigrationVersion(t, rollupsUnknownVersion)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))
	prevDefs := viewDefs143(ctx, t, pool)

	// Two collection instants inside the current month, whose partition the
	// migrations create, and inside provider_savings_summary's 90-day window.
	now := time.Now().UTC()
	t1 := time.Date(now.Year(), now.Month(), 1, 1, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	insertSnapshots143(ctx, t, pool, []snapshotRow143{
		// One region known, one with no provider figure at the same instant.
		{"acct-mixed", t1, "us-east-1", cost143(4000), cost143(4000)},
		{"acct-mixed", t1, "eu-west-1", nil, nil},
		// A real, known zero in every row.
		{"acct-zero", t1, "us-east-1", cost143(0), cost143(0)},
		{"acct-zero", t1, "eu-west-1", cost143(0), cost143(0)},
		// Fully known: AVG(100+50, 200+100) = 225.
		{"acct-known", t1, "us-east-1", cost143(100), cost143(100)},
		{"acct-known", t1, "eu-west-1", cost143(50), cost143(50)},
		{"acct-known", t2, "us-east-1", cost143(200), cost143(200)},
		{"acct-known", t2, "eu-west-1", cost143(100), cost143(100)},
		// Known at the first instant, unknown at the second.
		{"acct-later", t1, "us-east-1", cost143(100), cost143(100)},
		{"acct-later", t2, "us-east-1", nil, nil},
	})

	// The previous definitions report the unknown rows as if they were absent.
	_, err := pool.Exec(ctx, `REFRESH MATERIALIZED VIEW monthly_savings_summary;
		REFRESH MATERIALIZED VIEW provider_savings_summary`)
	require.NoError(t, err)
	requireRollup143(t, "before, mixed", cost143(4000), readRollup143(ctx, t, pool, "acct-mixed"))
	requireRollup143(t, "before, later", cost143(100), readRollup143(ctx, t, pool, "acct-later"))

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, rollupsUnknownVersion))
	// The production refresh path (CONCURRENTLY) still works on the new views.
	_, err = pool.Exec(ctx, `SELECT refresh_savings_materialized_views()`)
	require.NoError(t, err)

	requireRollup143(t, "mixed", nil, readRollup143(ctx, t, pool, "acct-mixed"))
	requireRollup143(t, "later", nil, readRollup143(ctx, t, pool, "acct-later"))
	requireRollup143(t, "zero", cost143(0), readRollup143(ctx, t, pool, "acct-zero"))
	requireRollup143(t, "known", cost143(225), readRollup143(ctx, t, pool, "acct-known"))

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))
	assert.Equal(t, prevDefs, viewDefs143(ctx, t, pool), "down must restore the previous view definitions")
	requireRollup143(t, "after down, mixed", cost143(4000), readRollup143(ctx, t, pool, "acct-mixed"))

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, rollupsUnknownVersion))
	requireRollup143(t, "up again, mixed", nil, readRollup143(ctx, t, pool, "acct-mixed"))
}

// viewDefs143 returns the two views' definitions and unique indexes.
func viewDefs143(ctx context.Context, t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	defs := map[string]string{}
	for _, view := range []string{"monthly_savings_summary", "provider_savings_summary"} {
		var def, idx string
		require.NoError(t, pool.QueryRow(ctx, `SELECT pg_get_viewdef($1::text::regclass),
			(SELECT string_agg(indexdef, ';' ORDER BY indexname) FROM pg_indexes WHERE tablename = $1::text)`,
			view).Scan(&def, &idx))
		defs[view] = def + "\n" + idx
	}
	return defs
}

// TestMigration_SavingsRollupsPropagateUnknown_ConcurrentRefresh covers the
// deploy-time race: the collector's refresh_savings_materialized_views() runs
// while 000143 is dropping and recreating the views. The refresh must wait for
// the migration and then refresh the new views, not fail on the dropped ones.
func TestMigration_SavingsRollupsPropagateUnknown_ConcurrentRefresh(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { require.NoError(t, container.Cleanup(context.Background())) })
	pool, path := container.DB.Pool(), getMigrationsPath()
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, previousMigrationVersion(t, rollupsUnknownVersion)))

	now := time.Now().UTC()
	t1 := time.Date(now.Year(), now.Month(), 1, 1, 0, 0, 0, time.UTC)
	insertSnapshots143(ctx, t, pool, []snapshotRow143{
		{"acct-mixed", t1, "us-east-1", cost143(4000), cost143(4000)},
		{"acct-mixed", t1, "eu-west-1", nil, nil},
	})

	// A reader holding provider_savings_summary stops the migration after it
	// has replaced monthly_savings_summary, with that view still locked.
	blocker, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blocker.Rollback(context.Background()) })
	_, err = blocker.Exec(ctx, `SELECT count(*) FROM provider_savings_summary`)
	require.NoError(t, err)

	migrated := make(chan error, 1)
	go func() { migrated <- migrations.MigrateToVersion(ctx, pool, path, rollupsUnknownVersion) }()
	// pg_stat_activity keeps only the start of the migration's text, so match
	// on its header comment.
	testhelpers.WaitForLockWait(ctx, t, pool, "-- 000143:%")

	refreshed := make(chan error, 1)
	go func() {
		_, err := pool.Exec(ctx, `SELECT refresh_savings_materialized_views()`)
		refreshed <- err
	}()
	testhelpers.WaitForLockWait(ctx, t, pool, "SELECT refresh_savings_materialized_views()")

	require.NoError(t, blocker.Rollback(ctx))
	require.NoError(t, testhelpers.Await(t, migrated))
	require.NoError(t, testhelpers.Await(t, refreshed), "a refresh queued behind the migration must succeed")
	requireRollup143(t, "after racing refresh", nil, readRollup143(ctx, t, pool, "acct-mixed"))
}
