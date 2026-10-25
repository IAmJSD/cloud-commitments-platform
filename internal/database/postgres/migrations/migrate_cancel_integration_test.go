//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// migrationDB returns a pool on a fresh, empty database so each test controls
// its own schema_migrations state. CUDLY_TEST_ADMIN_DSN points at an existing
// server (used where Docker is unavailable); otherwise the test gets an empty
// database on the package's shared container.
func migrationDB(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()

	adminDSN := os.Getenv("CUDLY_TEST_ADMIN_DSN")
	if adminDSN == "" {
		c := testhelpers.RequireEmptyDatabase(ctx, t)
		return c.DB.Pool(), c.Config.Database
	}

	admin, err := pgxpool.New(ctx, adminDSN)
	require.NoError(t, err)
	t.Cleanup(admin.Close)

	name := fmt.Sprintf("cancel_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, "CREATE DATABASE "+name)
	require.NoError(t, err)

	cfg, err := pgxpool.ParseConfig(adminDSN)
	require.NoError(t, err)
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool, name
}

func migrationDir(t *testing.T, upSQL string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "000001_blocked.up.sql"), []byte(upSQL), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "000001_blocked.down.sql"), []byte("SELECT 1;"), 0o600))
	return dir
}

// session is a dedicated connection that holds locks until released, standing
// in for the "other" process that blocks the migration.
func session(t *testing.T, pool *pgxpool.Pool) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(ctx) })
	return conn
}

func runBounded(t *testing.T, pool *pgxpool.Pool, dir string, timeout, bound time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- migrations.RunMigrations(ctx, pool, dir, "", "") }()
	select {
	case err := <-done:
		return err
	case <-time.After(bound):
		t.Fatalf("RunMigrations still blocked %s after its %s deadline", bound, timeout)
		return nil
	}
}

// requireNoBackend waits for the server to drop every active backend whose
// current query starts with prefix in database db.
func requireNoBackend(t *testing.T, pool *pgxpool.Pool, db, prefix string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var n int
		err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND state = 'active' AND query LIKE $2 AND pid <> pg_backend_pid()`,
			db, prefix+"%").Scan(&n)
		return err == nil && n == 0
	}, 5*time.Second, 100*time.Millisecond, "a backend running %q survived cancellation", prefix)
}

func TestRunMigrations_DeadlineStopsMigrationSQLBlockedByForeignLock(t *testing.T) {
	// Not parallel: asserts a 1s deadline is honored within fixed wall-clock bounds.
	pool, db := migrationDB(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, "CREATE TABLE locktarget (id int)")
	require.NoError(t, err)

	holder := session(t, pool)
	tx, err := holder.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, "LOCK TABLE locktarget IN ACCESS EXCLUSIVE MODE")
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(ctx) })

	dir := migrationDir(t, "ALTER TABLE locktarget ADD COLUMN extra int;")
	start := time.Now()
	err = runBounded(t, pool, dir, time.Second, 10*time.Second)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.DeadlineExceeded), "want deadline error, got %v", err)
	require.Less(t, time.Since(start), 8*time.Second)

	requireNoBackend(t, pool, db, "ALTER TABLE locktarget")

	// The foreign lock is still held and the interrupted DDL left no trace.
	require.NoError(t, tx.Rollback(ctx))
	var cols int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns WHERE table_name = 'locktarget' AND column_name = 'extra'`).Scan(&cols))
	require.Zero(t, cols, "interrupted migration must roll back")

	// Dirty-schema safety: the interrupted version stays recorded as dirty.
	var version int
	var dirty bool
	require.NoError(t, pool.QueryRow(ctx, "SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty))
	require.Equal(t, 1, version)
	require.True(t, dirty)
}

func TestRunMigrations_DeadlineStopsAdvisoryLockWait(t *testing.T) {
	// Not parallel: asserts a 1s deadline is honored within a fixed wall-clock bound.
	pool, db := migrationDB(t)
	ctx := context.Background()

	// Same ID the stock driver derives: database "/<name>", schema public.
	aid, err := database.GenerateAdvisoryLockId("/"+db, "public", "schema_migrations")
	require.NoError(t, err)
	holder := session(t, pool)
	_, err = holder.Exec(ctx, "SELECT pg_advisory_lock($1::bigint)", aid)
	require.NoError(t, err)

	dir := migrationDir(t, "SELECT 1;")
	err = runBounded(t, pool, dir, time.Second, 10*time.Second)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.DeadlineExceeded), "want deadline error, got %v", err)

	requireNoBackend(t, pool, db, "SELECT pg_advisory_lock")

	var held bool
	require.NoError(t, holder.QueryRow(ctx, "SELECT pg_advisory_unlock($1::bigint)", aid).Scan(&held))
	require.True(t, held, "the foreign session must still own its lock")
}

func TestRunMigrations_CancelStopsSetupBlockedOnVersionTable(t *testing.T) {
	// Not parallel: asserts a 1s deadline is honored within a fixed wall-clock bound.
	pool, db := migrationDB(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, "CREATE TABLE schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)")
	require.NoError(t, err)

	holder := session(t, pool)
	tx, err := holder.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, "LOCK TABLE schema_migrations IN ACCESS EXCLUSIVE MODE")
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(ctx) })

	dir := migrationDir(t, "SELECT 1;")
	err = runBounded(t, pool, dir, time.Second, 10*time.Second)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.DeadlineExceeded), "want deadline error, got %v", err)

	requireNoBackend(t, pool, db, "SELECT version, dirty FROM")
}

func TestRunMigrations_CompletesWithinDeadline(t *testing.T) {
	// Not parallel: asserts the run finishes inside a fixed wall-clock deadline.
	pool, _ := migrationDB(t)
	dir := migrationDir(t, "CREATE TABLE widgets (id int);")

	require.NoError(t, runBounded(t, pool, dir, 30*time.Second, 40*time.Second))

	var version int
	var dirty bool
	require.NoError(t, pool.QueryRow(context.Background(), "SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty))
	require.Equal(t, 1, version)
	require.False(t, dirty)
}

// TestRunMigrations_AppliesFullSchemaThroughCancelableMigrator runs the real
// migration set through the context-aware constructor and checks a second run
// is a clean no-op, so the replacement for migrate.New changes nothing for the
// normal path.
func TestRunMigrations_AppliesFullSchemaThroughCancelableMigrator(t *testing.T) {
	t.Parallel()
	pool, _ := migrationDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	require.NoError(t, migrations.RunMigrations(ctx, pool, getMigrationsPath(), "", ""))
	require.NoError(t, migrations.RunMigrations(ctx, pool, getMigrationsPath(), "", ""))

	var dirty bool
	require.NoError(t, pool.QueryRow(ctx, "SELECT dirty FROM schema_migrations").Scan(&dirty))
	require.False(t, dirty)
}
