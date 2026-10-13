//go:build integration
// +build integration

package config

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const settledAtMigration = 117

// TestPostgresStoreDB_LadderSettledAt runs against a dedicated database named
// pr842a_* (LADDER_SETTLED_TEST_DATABASE_URL). It covers migration 000117
// (down, up, idempotent re-run, legacy rows keep NULL), the stamp written by
// TransitionLadderTrancheTx, the CHECK that rejects an unstamped completed
// row, and the boundaries of settledLadderCommit.
func TestPostgresStoreDB_LadderSettledAt(t *testing.T) {
	dsn := os.Getenv("LADDER_SETTLED_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set LADDER_SETTLED_TEST_DATABASE_URL to a dedicated pr842a_* test database")
	}
	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(strings.TrimPrefix(parsed.Path, "/"), "pr842a_"), "database must be dedicated to PR-A verification")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()
	path := getTestMigrationsPath()
	require.NoError(t, migrations.RunMigrations(ctx, pool, path, "", ""))
	_, err = pool.Exec(ctx, `TRUNCATE ladder_tranches, ladder_runs, ladder_configs, cloud_accounts CASCADE`)
	require.NoError(t, err, "the dedicated database is reset so the test is re-runnable")
	store := &PostgresStore{db: pool}
	cfgID := seedLadderConfig(ctx, t, store)

	insert := func(status ladder.TrancheStatus, amount float64, settled *time.Time) string {
		t.Helper()
		tr := lifecycleTranche(cfgID, amount, status, time.Now().UTC().Add(time.Hour))
		tr.SettledAt = settled
		require.NoError(t, store.SaveLadderTranches(ctx, []LadderTrancheDB{tr}))
		return tr.ID
	}
	settledAt := func(id string) *time.Time {
		t.Helper()
		var at *time.Time
		require.NoError(t, pool.QueryRow(ctx, `SELECT settled_at FROM ladder_tranches WHERE id = $1`, id).Scan(&at))
		return at
	}

	t.Run("transition stamps only completed", func(t *testing.T) {
		completed, failed, skipped := insert(ladder.TrancheStatusFired, 1, nil), insert(ladder.TrancheStatusFired, 1, nil), insert(ladder.TrancheStatusScheduled, 1, nil)
		before := time.Now().Add(-time.Second)
		moved, err := store.TransitionLadderTranche(ctx, completed, ladder.TrancheStatusFired, ladder.TrancheStatusCompleted, "commitment_visible")
		require.NoError(t, err)
		require.True(t, moved)
		after := time.Now().Add(time.Second)
		at := settledAt(completed)
		require.NotNil(t, at, "completed must be stamped")
		assert.True(t, at.After(before) && at.Before(after), "stamp %s must be the transition time", at)

		_, err = store.TransitionLadderTranche(ctx, failed, ladder.TrancheStatusFired, ladder.TrancheStatusFailed, "boom")
		require.NoError(t, err)
		_, err = store.TransitionLadderTranche(ctx, skipped, ladder.TrancheStatusScheduled, ladder.TrancheStatusCancelled, "no")
		require.NoError(t, err)
		assert.Nil(t, settledAt(failed))
		assert.Nil(t, settledAt(skipped))
	})

	t.Run("a lost compare-and-set leaves the stamp alone", func(t *testing.T) {
		id := insert(ladder.TrancheStatusFired, 1, nil)
		moved, err := store.TransitionLadderTranche(ctx, id, ladder.TrancheStatusFired, ladder.TrancheStatusCompleted, "x")
		require.NoError(t, err)
		require.True(t, moved)
		first := settledAt(id)
		moved, err = store.TransitionLadderTranche(ctx, id, ladder.TrancheStatusFired, ladder.TrancheStatusCompleted, "x")
		require.NoError(t, err)
		assert.False(t, moved)
		assert.True(t, first.Equal(*settledAt(id)))
	})

	t.Run("constraint rejects an unstamped completed row", func(t *testing.T) {
		tr := lifecycleTranche(cfgID, 1, ladder.TrancheStatusCompleted, time.Now().UTC())
		tr.SettledAt = nil
		err := store.SaveLadderTranches(ctx, []LadderTrancheDB{tr})
		requireCheckViolation(t, err)
		fired := insert(ladder.TrancheStatusFired, 1, nil)
		_, err = pool.Exec(ctx, `UPDATE ladder_tranches SET status = 'completed' WHERE id = $1`, fired)
		requireCheckViolation(t, err)
	})

	t.Run("settledLadderCommit boundaries", func(t *testing.T) {
		otherCfg := seedLadderConfigWithExtID(ctx, t, store, "333333333333")
		anchor := time.Now().UTC().Truncate(time.Microsecond)
		add := func(status ladder.TrancheStatus, amount float64, settled *time.Time) {
			tr := lifecycleTranche(otherCfg, amount, status, anchor.Add(time.Hour))
			tr.SettledAt = settled
			require.NoError(t, store.SaveLadderTranches(ctx, []LadderTrancheDB{tr}))
		}
		before, equal, after := anchor.Add(-time.Microsecond), anchor, anchor.Add(time.Microsecond)
		sum := func() float64 {
			v, err := settledLadderCommit(ctx, pool, otherCfg, anchor)
			require.NoError(t, err)
			return v
		}
		assert.InDelta(t, 0, sum(), 1e-9, "no rows")
		add(ladder.TrancheStatusCompleted, 1, &before)
		add(ladder.TrancheStatusCompleted, 2, &equal)
		assert.InDelta(t, 0, sum(), 1e-9, "settled at or before the anchor is already in the existing commitment")
		add(ladder.TrancheStatusCompleted, 4, &after)
		assert.InDelta(t, 4, sum(), 1e-9, "one microsecond after counts")
		add(ladder.TrancheStatusFailed, 8, nil)
		add(ladder.TrancheStatusCancelled, 16, nil)
		add(ladder.TrancheStatusFired, 32, nil)
		add(ladder.TrancheStatusScheduled, 64, nil)
		assert.InDelta(t, 4, sum(), 1e-9, "only completed rows count")
		assert.InDelta(t, 0, mustSettled(ctx, t, pool, cfgID, time.Now().Add(time.Hour)), 1e-9, "other config's completions after the anchor only")
	})

	t.Run("migration down and up", func(t *testing.T) {
		require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, settledAtMigration-1))
		assert.Equal(t, 0, settledColumns(ctx, t, pool), "down drops the column")
		assert.Equal(t, 0, settledConstraints(ctx, t, pool), "down drops the constraint")

		legacy := lifecycleTranche(cfgID, 5, ladder.TrancheStatusCompleted, time.Now().UTC())
		_, err := pool.Exec(ctx, `INSERT INTO ladder_tranches (id, config_id, layer_type, amount_usd_hr, term, payment_option, scheduled_date, status)
			VALUES ($1, $2, $3, 5, $4, $5, $6, 'completed')`, legacy.ID, cfgID, string(legacy.LayerType), string(legacy.Term), string(legacy.PaymentOption), legacy.ScheduledDate)
		require.NoError(t, err)

		require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, settledAtMigration))
		assert.Equal(t, 1, settledColumns(ctx, t, pool))
		var validated bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT convalidated FROM pg_constraint WHERE conname = 'ladder_tranches_completed_has_settled_at'`).Scan(&validated))
		assert.False(t, validated, "the constraint is NOT VALID so legacy rows stay")
		assert.Nil(t, settledAt(legacy.ID), "a legacy completed row keeps NULL")
		got, err := settledLadderCommit(ctx, pool, cfgID, time.Now().Add(24*time.Hour))
		require.NoError(t, err)
		assert.GreaterOrEqual(t, got, 5.0, "a NULL legacy completed row is counted whatever the anchor")

		up, err := os.ReadFile(getTestMigrationsPath() + "/000117_ladder_tranches_settled_at.up.sql")
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(up))
		require.NoError(t, err, "the up statement must be idempotent")
		assert.Equal(t, 1, settledConstraints(ctx, t, pool), "a re-run adds no second constraint")
	})
}

func mustSettled(ctx context.Context, t *testing.T, pool *pgxpool.Pool, cfgID string, anchor time.Time) float64 {
	t.Helper()
	v, err := settledLadderCommit(ctx, pool, cfgID, anchor)
	require.NoError(t, err)
	return v
}

func requireCheckViolation(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "23514", pgErr.Code)
	assert.Equal(t, "ladder_tranches_completed_has_settled_at", pgErr.ConstraintName)
}

func settledColumns(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'ladder_tranches' AND column_name = 'settled_at'`).Scan(&n))
	return n
}

func settledConstraints(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conname = 'ladder_tranches_completed_has_settled_at'`).Scan(&n))
	return n
}
