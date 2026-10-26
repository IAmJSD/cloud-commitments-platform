//go:build integration
// +build integration

package testhelpers

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	code := m.Run()
	if err := TerminateSharedPostgres(); err != nil {
		log.Printf("terminate shared postgres container: %v", err)
	}
	os.Exit(code)
}

func sharedTestMigrationsPath() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "migrations")
}

// latestMigrationVersion is the highest NNNNNN prefix among the up migrations,
// i.e. the version a fully migrated database must report.
func latestMigrationVersion(t *testing.T) int64 {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(sharedTestMigrationsPath(), "*.up.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "no up migrations found")
	var latest int64
	for _, f := range files {
		prefix, _, _ := strings.Cut(filepath.Base(f), "_")
		v, err := strconv.ParseInt(prefix, 10, 64)
		require.NoError(t, err, "migration file %s has no numeric prefix", f)
		latest = max(latest, v)
	}
	return latest
}

func requireFullyMigrated(ctx context.Context, t *testing.T, db *database.Connection, want int64) {
	t.Helper()
	var version int64
	var dirty bool
	require.NoError(t, db.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty))
	assert.Equal(t, want, version, "per-test database must carry every migration")
	assert.False(t, dirty, "per-test database must not be cloned from a dirty template")
}

func tableExists(ctx context.Context, t *testing.T, db *database.Connection, table string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists))
	return exists
}

// TestRequireMigratedDatabase_IsolatedCopiesOnOneContainer pins what makes the
// shared container safe to share (issue #982): every caller gets a distinct,
// fully migrated database on the same server, and nothing one test writes
// reaches another test or the template later tests are cloned from.
func TestRequireMigratedDatabase_IsolatedCopiesOnOneContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	want := latestMigrationVersion(t)

	a := RequireMigratedDatabase(ctx, t, sharedTestMigrationsPath())
	b := RequireMigratedDatabase(ctx, t, sharedTestMigrationsPath())

	assert.NotEqual(t, a.Config.Database, b.Config.Database, "each caller needs a database of its own")
	assert.Equal(t, a.Config.Host, b.Config.Host, "both databases live on the one shared server")
	assert.Equal(t, a.Config.Port, b.Config.Port, "both databases live on the one shared server")
	assert.Nil(t, a.Container, "the shared container is not the caller's to terminate")
	requireFullyMigrated(ctx, t, a.DB, want)
	requireFullyMigrated(ctx, t, b.DB, want)

	// A table and a row written in a are invisible in b, and in a database
	// handed out afterwards: the template stayed untouched.
	_, err := a.DB.Exec(ctx, `CREATE TABLE isolation_probe (id int)`)
	require.NoError(t, err)
	tag, err := a.DB.Exec(ctx, `UPDATE global_config SET updated_at = '2001-02-03T04:05:06Z' WHERE id = 1`)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected(), "migrations seed the global_config row this probe rewrites")

	c := RequireMigratedDatabase(ctx, t, sharedTestMigrationsPath())
	for name, db := range map[string]*database.Connection{"sibling": b.DB, "later clone": c.DB} {
		assert.False(t, tableExists(ctx, t, db, "isolation_probe"), "%s sees a table another test created", name)
		var leaked bool
		require.NoError(t, db.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM global_config WHERE updated_at = '2001-02-03T04:05:06Z')`).Scan(&leaked))
		assert.False(t, leaked, "%s sees a row another test wrote", name)
	}
}

// TestRequireMigratedDatabase_TemplateRefusesConnections pins the seal on the
// template. A connection to it would make every later clone fail, and a write
// through one would leak into every later test's starting state.
func TestRequireMigratedDatabase_TemplateRefusesConnections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := RequireMigratedDatabase(ctx, t, sharedTestMigrationsPath())

	shared.mu.Lock()
	tmpl := shared.templates[templateKey(sharedTestMigrationsPath())]
	shared.mu.Unlock()
	require.NoError(t, tmpl.err)
	require.NotEmpty(t, tmpl.name)

	conn, err := pgx.Connect(ctx, connString(db.Config, tmpl.name))
	if err == nil {
		_ = conn.Close(ctx)
	}
	require.Error(t, err, "the template must not accept connections")
	assert.Contains(t, err.Error(), "not currently accepting connections")
}

// TestRequireMigratedDatabase_DropsDatabaseWhenTestEnds checks that a
// per-test database does not outlive its test, even when the test leaked a
// connection to it: without FORCE that connection would block the drop.
func TestRequireMigratedDatabase_DropsDatabaseWhenTestEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var name string
	var cfg *database.Config
	t.Run("uses and leaks", func(t *testing.T) {
		db := RequireMigratedDatabase(ctx, t, sharedTestMigrationsPath())
		name, cfg = db.Config.Database, db.Config
		leaked, err := pgx.Connect(ctx, connString(cfg, name))
		require.NoError(t, err)
		require.NoError(t, leaked.Ping(ctx)) // never closed, on purpose
	})
	require.NotEmpty(t, name)

	admin, err := pgx.Connect(ctx, connString(cfg, "postgres"))
	require.NoError(t, err)
	defer func() { _ = admin.Close(ctx) }()
	var exists bool
	require.NoError(t, admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists))
	assert.False(t, exists, "database %s outlived its test", name)
}

// TestRequireMigratedDatabase_ConcurrentFirstUse starts from no shared server
// and has many tests ask for a database at once, as parallel tests do. They
// must agree on one container and one template and still each get a distinct,
// fully migrated database. Run it under -race.
func TestRequireMigratedDatabase_ConcurrentFirstUse(t *testing.T) {
	require.NoError(t, TerminateSharedPostgres(), "reset to a cold start")
	want := latestMigrationVersion(t)

	const callers = 8
	var mu sync.Mutex
	names := map[string]bool{}
	servers := map[string]bool{}
	t.Run("group", func(t *testing.T) {
		for i := 0; i < callers; i++ {
			t.Run(fmt.Sprintf("caller-%d", i), func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				db := RequireMigratedDatabase(ctx, t, sharedTestMigrationsPath())
				requireFullyMigrated(ctx, t, db.DB, want)

				mu.Lock()
				defer mu.Unlock()
				names[db.Config.Database] = true
				servers[fmt.Sprintf("%s:%d", db.Config.Host, db.Config.Port)] = true
			})
		}
	})

	assert.Len(t, names, callers, "every caller needs a database of its own")
	assert.Len(t, servers, 1, "concurrent first callers must share one container")
	shared.mu.Lock()
	defer shared.mu.Unlock()
	assert.Len(t, shared.templates, 1, "concurrent first callers must share one template")
}

// TestRequireMigratedDatabase_CanceledFirstCallerDoesNotPoisonTemplate has the
// caller that triggers the template build arrive with its context already
// canceled. The build's outcome is cached for the whole binary, so it must not
// depend on that caller's context: later callers get a migrated database.
func TestRequireMigratedDatabase_CanceledFirstCallerDoesNotPoisonTemplate(t *testing.T) {
	require.NoError(t, TerminateSharedPostgres(), "reset to a cold start, so this caller builds the template")

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	// The caller's own CREATE DATABASE may fail on its dead context; that
	// failure is the caller's alone and is not what this test is about.
	if name, _, err := shared.newDatabase(canceled, t, sharedTestMigrationsPath()); err == nil {
		t.Cleanup(func() { shared.dropDatabase(t, name) })
	}

	shared.mu.Lock()
	tmpl, built := shared.templates[templateKey(sharedTestMigrationsPath())]
	shared.mu.Unlock()
	require.True(t, built, "the canceled first caller must still have built the template")
	require.NoError(t, tmpl.err, "a canceled first caller must not cache a failed template")

	ctx, stop := context.WithTimeout(context.Background(), 2*time.Minute)
	defer stop()
	db := RequireMigratedDatabase(ctx, t, sharedTestMigrationsPath())
	requireFullyMigrated(ctx, t, db.DB, latestMigrationVersion(t))
}

// TestRequireEmptyDatabase_CleanupAfterTerminate stops the shared server while
// a test still holds a database on it, as a TestMain teardown racing a late
// cleanup would. The cleanup must find nothing to drop, not fail or panic.
func TestRequireEmptyDatabase_CleanupAfterTerminate(t *testing.T) {
	passed := t.Run("terminates the server mid-test", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		db := RequireEmptyDatabase(ctx, t)
		require.NoError(t, db.DB.Ping(ctx))
		require.NoError(t, TerminateSharedPostgres())
	})
	assert.True(t, passed, "cleanup after the shared server stopped must not fail the test")
}

// TestRequireSharedDatabase_ProbesDockerOnlyUntilServerIsUp pins that the
// Docker probe runs on a cold start but not once the server is up. A probe
// that times out under load would skip a test whose database is reachable,
// a false green.
func TestRequireSharedDatabase_ProbesDockerOnlyUntilServerIsUp(t *testing.T) {
	require.NoError(t, TerminateSharedPostgres(), "reset to a cold start")
	probes := 0
	orig := providerProbe
	providerProbe = func(t *testing.T) { probes++; orig(t) }
	t.Cleanup(func() { providerProbe = orig })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	RequireEmptyDatabase(ctx, t)
	assert.Equal(t, 1, probes, "a cold start must probe Docker before starting the server")
	RequireEmptyDatabase(ctx, t)
	assert.Equal(t, 1, probes, "a running server must not be probed again")
}

// connString points cfg's server and credentials at dbName.
func connString(cfg *database.Config, dbName string) string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(cfg.User, cfg.Password),
		Host:     net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Path:     "/" + dbName,
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

// userRelations counts the relations (tables, views, sequences, indexes, ...)
// outside the system schemas: zero for a database nothing has touched.
func userRelations(ctx context.Context, t *testing.T, db *database.Connection) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(ctx, `
		SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg_toast%'`).Scan(&n))
	return n
}

// requireEmpty asserts db is what CREATE DATABASE ... TEMPLATE template0 makes:
// no user relations and no extension beyond the built-in plpgsql.
func requireEmpty(ctx context.Context, t *testing.T, db *database.Connection, name string) {
	t.Helper()
	assert.Zero(t, userRelations(ctx, t, db), "%s must have no tables", name)
	assert.False(t, tableExists(ctx, t, db, "schema_migrations"), "%s must not be migrated", name)
	rows, err := db.Query(ctx, `SELECT extname FROM pg_extension ORDER BY extname`)
	require.NoError(t, err)
	extensions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	assert.Equal(t, []string{"plpgsql"}, extensions, "%s must have only the built-in extension", name)
}

// TestRequireEmptyDatabase_IsEmpty pins the contract the migration tests rely
// on: the database starts with nothing in it, even after a migrated template
// exists on the same server, and it is the caller's alone.
func TestRequireEmptyDatabase_IsEmpty(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// A migrated database first, so a template with every table exists on the
	// server and an empty database cloned from the wrong template would show it.
	migrated := RequireMigratedDatabase(ctx, t, sharedTestMigrationsPath())
	require.True(t, tableExists(ctx, t, migrated.DB, "schema_migrations"))

	db := RequireEmptyDatabase(ctx, t)
	assert.Nil(t, db.Container, "the shared container is not the caller's to terminate")
	assert.NotEqual(t, migrated.Config.Database, db.Config.Database)
	assert.Equal(t, migrated.Config.Host, db.Config.Host, "empty databases live on the one shared server")
	assert.Equal(t, migrated.Config.Port, db.Config.Port, "empty databases live on the one shared server")
	requireEmpty(ctx, t, db.DB, "a new empty database")

	var current string
	require.NoError(t, db.DB.QueryRow(ctx, `SELECT current_database()`).Scan(&current))
	assert.Equal(t, db.Config.Database, current, "Config and DB must point at the same database")
}

// TestRequireEmptyDatabase_IsolatedFromOtherDatabases migrates one empty
// database all the way, as a migration test does, and checks that neither a
// sibling nor a database handed out afterwards sees any of it, and that the
// migrated template is untouched by it.
func TestRequireEmptyDatabase_IsolatedFromOtherDatabases(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	want := latestMigrationVersion(t)

	a := RequireEmptyDatabase(ctx, t)
	b := RequireEmptyDatabase(ctx, t)
	assert.NotEqual(t, a.Config.Database, b.Config.Database, "each caller needs a database of its own")

	require.NoError(t, migrations.RunMigrations(ctx, a.DB.Pool(), sharedTestMigrationsPath(), "", ""),
		"the full chain must apply to an empty database")
	requireFullyMigrated(ctx, t, a.DB, want)
	_, err := a.DB.Exec(ctx, `CREATE TABLE isolation_probe (id int)`)
	require.NoError(t, err)

	c := RequireEmptyDatabase(ctx, t)
	requireEmpty(ctx, t, b.DB, "a sibling empty database")
	requireEmpty(ctx, t, c.DB, "a later empty database")

	m := RequireMigratedDatabase(ctx, t, sharedTestMigrationsPath())
	assert.False(t, tableExists(ctx, t, m.DB, "isolation_probe"), "a migrated database sees a table an empty one created")
}

// TestRequireEmptyDatabase_DropsDatabaseWhenTestEnds checks that an empty
// database does not outlive its test, even with a leaked connection to it.
func TestRequireEmptyDatabase_DropsDatabaseWhenTestEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var name string
	var cfg *database.Config
	t.Run("uses and leaks", func(t *testing.T) {
		db := RequireEmptyDatabase(ctx, t)
		name, cfg = db.Config.Database, db.Config
		leaked, err := pgx.Connect(ctx, connString(cfg, name))
		require.NoError(t, err)
		require.NoError(t, leaked.Ping(ctx)) // never closed, on purpose
	})
	require.NotEmpty(t, name)

	admin, err := pgx.Connect(ctx, connString(cfg, "postgres"))
	require.NoError(t, err)
	defer func() { _ = admin.Close(ctx) }()
	var exists bool
	require.NoError(t, admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists))
	assert.False(t, exists, "database %s outlived its test", name)
}

// TestRequireEmptyDatabase_ConcurrentFirstUse has parallel callers ask for an
// empty database from a cold start. They must share one container, each get a
// distinct empty database, and not cause a template to be migrated: nobody
// asked for one.
func TestRequireEmptyDatabase_ConcurrentFirstUse(t *testing.T) {
	require.NoError(t, TerminateSharedPostgres(), "reset to a cold start")

	const callers = 8
	var mu sync.Mutex
	names := map[string]bool{}
	servers := map[string]bool{}
	t.Run("group", func(t *testing.T) {
		for i := 0; i < callers; i++ {
			t.Run(fmt.Sprintf("caller-%d", i), func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				db := RequireEmptyDatabase(ctx, t)
				// Each caller writes to its own database; a shared one would
				// make the second CREATE fail.
				_, err := db.DB.Exec(ctx, `CREATE TABLE concurrency_probe (id int)`)
				require.NoError(t, err)

				mu.Lock()
				defer mu.Unlock()
				names[db.Config.Database] = true
				servers[fmt.Sprintf("%s:%d", db.Config.Host, db.Config.Port)] = true
			})
		}
	})

	assert.Len(t, names, callers, "every caller needs a database of its own")
	assert.Len(t, servers, 1, "concurrent first callers must share one container")
	shared.mu.Lock()
	defer shared.mu.Unlock()
	assert.Empty(t, shared.templates, "an empty database must not migrate a template")
}
