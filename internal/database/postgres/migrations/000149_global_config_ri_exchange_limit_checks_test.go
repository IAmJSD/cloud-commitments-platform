//go:build integration

package migrations_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
)

const (
	m149Version   = 149
	m149UpFile    = "000149_global_config_ri_exchange_limit_checks.up.sql"
	m149DownFile  = "000149_global_config_ri_exchange_limit_checks.down.sql"
	m149PerExCap  = "chk_global_config_ri_exchange_max_per_exchange_usd_range"
	m149DailyCap  = "chk_global_config_ri_exchange_max_daily_usd_range"
	m149Threshold = "chk_global_config_ri_exchange_utilization_threshold_range"
)

// m149ValidatedState maps each 000149 constraint present on global_config to
// whether it is validated.
func m149ValidatedState(ctx context.Context, t *testing.T, pool *pgxpool.Pool) map[string]bool {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT conname, convalidated FROM pg_constraint
		WHERE conrelid = 'global_config'::regclass AND conname = ANY($1)`,
		[]string{m149PerExCap, m149DailyCap, m149Threshold})
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		var validated bool
		require.NoError(t, rows.Scan(&name, &validated))
		out[name] = validated
	}
	require.NoError(t, rows.Err())
	return out
}

// m149Set writes one RI-exchange column of the single global_config row.
// value is text so 'NaN' and 'Infinity' can be written.
func m149Set(ctx context.Context, pool *pgxpool.Pool, column, value string) error {
	_, err := pool.Exec(ctx, `UPDATE global_config SET `+column+` = $1::float8 WHERE id = 1`, value)
	return err
}

func m149RequireCheckViolation(t *testing.T, err error, constraint, msg string) {
	t.Helper()
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "%s: want a PostgreSQL error, got %v", msg, err)
	assert.Equal(t, "23514", pgErr.Code, "%s: want check_violation", msg)
	assert.Equal(t, constraint, pgErr.ConstraintName, msg)
}

func m149MigrateToPrevious(ctx context.Context, t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool, path := container.DB.Pool(), getMigrationsPath()
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, previousMigrationVersion(t, m149Version)))
	_, err := pool.Exec(ctx, `INSERT INTO global_config (id) VALUES (1) ON CONFLICT (id) DO NOTHING`)
	require.NoError(t, err)
	return pool, path
}

// TestMigration149_EnforcesRIExchangeLimitRanges: on a clean row every
// constraint is added and validated, out-of-range writes (negative, NaN,
// infinite caps; a threshold outside [0, 100]) are refused and the bounds
// themselves are accepted. The file is idempotent and the down drops all three.
//
// Fail-before: at the previous version a negative cap is stored.
func TestMigration149_EnforcesRIExchangeLimitRanges(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	pool, path := m149MigrateToPrevious(ctx, t)

	require.NoError(t, m149Set(ctx, pool, "ri_exchange_max_daily_usd", "-1"), "precondition: no range check before 000149")
	require.NoError(t, m149Set(ctx, pool, "ri_exchange_max_daily_usd", "0"))

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, m149Version))
	all := map[string]bool{m149PerExCap: true, m149DailyCap: true, m149Threshold: true}
	require.Equal(t, all, m149ValidatedState(ctx, t, pool), "a clean row lets every constraint validate")

	refused := []struct{ column, value, constraint string }{
		{"ri_exchange_max_per_exchange_usd", "-1", m149PerExCap},
		{"ri_exchange_max_per_exchange_usd", "-0.000001", m149PerExCap},
		{"ri_exchange_max_per_exchange_usd", "Infinity", m149PerExCap},
		{"ri_exchange_max_per_exchange_usd", "NaN", m149PerExCap},
		{"ri_exchange_max_daily_usd", "-1", m149DailyCap},
		{"ri_exchange_max_daily_usd", "Infinity", m149DailyCap},
		{"ri_exchange_max_daily_usd", "NaN", m149DailyCap},
		{"ri_exchange_utilization_threshold", "-0.1", m149Threshold},
		{"ri_exchange_utilization_threshold", "100.1", m149Threshold},
		{"ri_exchange_utilization_threshold", "NaN", m149Threshold},
		{"ri_exchange_utilization_threshold", "-Infinity", m149Threshold},
	}
	for _, r := range refused {
		m149RequireCheckViolation(t, m149Set(ctx, pool, r.column, r.value), r.constraint, r.column+" = "+r.value)
	}
	accepted := []struct{ column, value string }{
		{"ri_exchange_max_per_exchange_usd", "0"},
		{"ri_exchange_max_per_exchange_usd", "1e12"},
		{"ri_exchange_max_daily_usd", "0"},
		{"ri_exchange_max_daily_usd", "25000.50"},
		{"ri_exchange_utilization_threshold", "0"},
		{"ri_exchange_utilization_threshold", "100"},
	}
	for _, a := range accepted {
		require.NoError(t, m149Set(ctx, pool, a.column, a.value), "%s = %s is in range", a.column, a.value)
	}

	up, err := os.ReadFile(filepath.Join(path, m149UpFile))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(up))
	require.NoError(t, err, "the up file is idempotent")
	require.Equal(t, all, m149ValidatedState(ctx, t, pool))

	down, err := os.ReadFile(filepath.Join(path, m149DownFile))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(down))
	require.NoError(t, err)
	require.Empty(t, m149ValidatedState(ctx, t, pool), "the down drops all three")
	require.NoError(t, m149Set(ctx, pool, "ri_exchange_max_daily_usd", "-1"), "no range check after the down")
}

// TestMigration149_KeepsAndReportsAnExistingBadRow: a row already out of range
// does not fail the migration and is not rewritten. Its constraints stay
// NOT VALID, which still refuses any write that keeps a bad value, and the
// clean column's constraint validates. Once the value is fixed, re-running
// the file validates the rest.
func TestMigration149_KeepsAndReportsAnExistingBadRow(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	pool, path := m149MigrateToPrevious(ctx, t)

	require.NoError(t, m149Set(ctx, pool, "ri_exchange_max_daily_usd", "-5"))
	require.NoError(t, m149Set(ctx, pool, "ri_exchange_utilization_threshold", "150"))

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, m149Version), "a bad row must not fail the migration")
	require.Equal(t, map[string]bool{m149PerExCap: true, m149DailyCap: false, m149Threshold: false},
		m149ValidatedState(ctx, t, pool))

	var daily, threshold float64
	require.NoError(t, pool.QueryRow(ctx, `SELECT ri_exchange_max_daily_usd, ri_exchange_utilization_threshold
		FROM global_config WHERE id = 1`).Scan(&daily, &threshold))
	assert.Equal(t, -5.0, daily, "the operator's value is kept, not rewritten")
	assert.Equal(t, 150.0, threshold)

	m149RequireCheckViolation(t, m149Set(ctx, pool, "ri_exchange_max_per_exchange_usd", "10"), m149DailyCap,
		"NOT VALID still checks every updated row, so a write keeping the bad cap is refused")
	m149RequireCheckViolation(t, m149Set(ctx, pool, "ri_exchange_max_daily_usd", "-6"), m149DailyCap, "a new bad value")

	_, err := pool.Exec(ctx, `UPDATE global_config SET ri_exchange_max_daily_usd = 0, ri_exchange_utilization_threshold = 95 WHERE id = 1`)
	require.NoError(t, err, "fixing both values in one write is accepted")
	up, err := os.ReadFile(filepath.Join(path, m149UpFile))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(up))
	require.NoError(t, err)
	require.Equal(t, map[string]bool{m149PerExCap: true, m149DailyCap: true, m149Threshold: true},
		m149ValidatedState(ctx, t, pool), "re-running the file after the fix validates the rest")
}

// TestMigration149_ConcurrentConfigWriter: a config write in flight when the
// migration starts holds the row; the migration waits for it, then sees the
// committed value. A bad value committed that way is kept and its constraint
// left NOT VALID rather than validated over a violating row or failing the
// migration, and the next bad write is refused.
func TestMigration149_ConcurrentConfigWriter(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	pool, path := m149MigrateToPrevious(ctx, t)

	writer, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Rollback(context.Background()) })
	_, err = writer.Exec(ctx, `UPDATE global_config SET ri_exchange_max_per_exchange_usd = -3 WHERE id = 1`)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- migrations.MigrateToVersion(ctx, pool, path, m149Version) }()
	// pg_stat_activity keeps only the first 1024 bytes of a query, so match
	// the file's first line rather than a constraint name further down.
	testhelpers.WaitForLockWait(ctx, t, pool, "-- Migration 000149:%")
	require.NoError(t, writer.Commit(ctx))
	require.NoError(t, testhelpers.Await(t, done), "the migration waits for the writer, then completes")

	require.Equal(t, map[string]bool{m149PerExCap: false, m149DailyCap: true, m149Threshold: true},
		m149ValidatedState(ctx, t, pool), "the value committed while the migration waited is seen")
	m149RequireCheckViolation(t, m149Set(ctx, pool, "ri_exchange_max_per_exchange_usd", "-4"), m149PerExCap,
		"a bad write after the migration is refused")
}
