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
	tmpl := shared.templates[filepath.Clean(sharedTestMigrationsPath())]
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
