package testhelpers

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
)

// sharedServer is one PostgreSQL container shared by every test in a test
// binary that asks for a migrated or an empty database (issue #982). Starting
// a container and applying the full migration chain for each test was most of
// the integration run. Here the container starts once, the migrations run once
// into a template database, and each test gets its own CREATE DATABASE ...
// TEMPLATE copy of it (of template0, for an empty one). Tests stay isolated
// from each other exactly as before: every test starts from a fresh database
// that nothing else writes to.
//
// One mutex covers all of it. Holding it while the container starts and the
// template migrates makes concurrent first callers wait for the one setup
// instead of racing to do their own, and holding it across CREATE DATABASE
// keeps clones of one template from contending with each other. Drops take it
// too, so TerminateSharedPostgres cannot pull the server out from under one.
type sharedServer struct {
	mu        sync.Mutex
	admin     *PostgresContainer // connected to the maintenance database, never handed to a test
	startErr  error
	templates map[string]templateDB // keyed by cleaned migrations path
	seq       int
}

// templateDB is the outcome of migrating one template. A migration failure is
// remembered, so every later test fails with it straight away rather than
// each re-running a chain that is known not to apply.
type templateDB struct {
	name string
	err  error
}

var shared sharedServer

// RequireMigratedDatabase gives t a database of its own on a PostgreSQL
// container shared by the whole test binary, with every migration under
// migrationsPath already applied. The database is dropped when t ends, and the
// connection pool is closed before that, so callers have nothing to clean up.
//
// Skips follow RequirePostgresContainer: only an unusable Docker provider
// skips. A container that will not start or a migration that will not apply
// fails the test (issue #1597).
//
// The returned value has no Container: it is shared, and terminating it is the
// job of TerminateSharedPostgres, called from the package's TestMain.
func RequireMigratedDatabase(ctx context.Context, t *testing.T, migrationsPath string) *PostgresContainer {
	t.Helper()
	require.NotEmpty(t, migrationsPath, "RequireMigratedDatabase needs a migrations path; use RequireEmptyDatabase for an unmigrated database")
	return requireSharedDatabase(ctx, t, migrationsPath)
}

// RequireEmptyDatabase gives t an empty database of its own on the same
// shared container RequireMigratedDatabase uses: no tables, no
// schema_migrations, nothing but what CREATE DATABASE ... TEMPLATE template0
// puts there. It is for tests whose subject is the migration chain itself (up,
// down, dirty, heal), which need to start from nothing and must not see a
// template another test migrated.
//
// Cleanup, skips and the missing Container are exactly as for
// RequireMigratedDatabase. Being per database, it is no substitute for a
// dedicated server where a test touches server-wide state (roles, ALTER
// SYSTEM, connection limits, other sessions' backends); see
// RequirePostgresContainer for those.
func RequireEmptyDatabase(ctx context.Context, t *testing.T) *PostgresContainer {
	t.Helper()
	return requireSharedDatabase(ctx, t, "")
}

// requireSharedDatabase creates the per-test database (cloned from the
// migrated template for migrationsPath, or from template0 when it is empty),
// registers its drop, and connects to it.
func requireSharedDatabase(ctx context.Context, t *testing.T, migrationsPath string) *PostgresContainer {
	t.Helper()

	// Probe Docker only until the shared server is up. Once it is, the server
	// answering is the evidence that matters, and a probe that times out under
	// load would otherwise skip a test whose database is reachable, the false
	// green #1597 is about.
	if !shared.running() {
		providerProbe(t)
	}

	name, cfg, err := shared.newDatabase(ctx, t, migrationsPath)
	require.NoError(t, err,
		"Docker is healthy but no PostgreSQL test database could be created on the shared server; "+
			"this is a real failure, not an environment without Docker")
	// Registered before connecting, so a database that was created but could not
	// be connected to is still dropped.
	t.Cleanup(func() { shared.dropDatabase(t, name) })

	conn, err := database.NewConnection(ctx, cfg, nil)
	require.NoError(t, err, "connect to per-test database %s", name)
	t.Cleanup(conn.Close) // runs before the drop: cleanups are last in, first out

	return &PostgresContainer{Config: cfg, DB: conn}
}

// providerProbe skips t when Docker is unusable. A variable so a test can
// observe when it runs.
var providerProbe = testcontainers.SkipIfProviderIsNotHealthy

// running reports whether the shared server is up.
func (s *sharedServer) running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.admin != nil
}

// TerminateSharedPostgres stops the shared container if one was started. Call
// it from TestMain after m.Run. Ryuk reaps the container if the process dies
// first, so this is tidiness rather than the only line of defense.
func TerminateSharedPostgres() error {
	shared.mu.Lock()
	defer shared.mu.Unlock()

	if shared.admin == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	err := shared.admin.Cleanup(ctx)
	shared.admin, shared.startErr, shared.templates = nil, nil, nil
	return err
}

// emptyTemplate is what RequireEmptyDatabase clones. template0 is the pristine
// database every PostgreSQL server ships and never accepts connections, so,
// unlike template1 (which CREATE DATABASE uses by default and buildTemplate
// clones), nothing can have added objects to it or be connected to it.
const emptyTemplate = "template0"

// newDatabase returns the name and connection config of a new database cloned
// from the migrated template for migrationsPath, or from template0 when
// migrationsPath is empty.
func (s *sharedServer) newDatabase(ctx context.Context, t *testing.T, migrationsPath string) (string, *database.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.start(ctx, t); err != nil {
		return "", nil, err
	}
	tmpl := emptyTemplate
	if migrationsPath != "" {
		var err error
		if tmpl, err = s.template(ctx, migrationsPath); err != nil {
			return "", nil, err
		}
	}

	s.seq++
	name := fmt.Sprintf("cudly_test_%d", s.seq)
	if err := s.exec(ctx, "CREATE DATABASE "+quoteIdent(name)+" TEMPLATE "+quoteIdent(tmpl)); err != nil {
		return "", nil, fmt.Errorf("clone template %s into %s: %w", tmpl, name, err)
	}
	return name, s.configFor(name), nil
}

// start brings the shared container up on first use. A failure is remembered
// so a broken Docker setup fails each test at once instead of each one waiting
// out its own startup timeout. Callers hold s.mu.
func (s *sharedServer) start(ctx context.Context, t *testing.T) error {
	if s.admin != nil || s.startErr != nil {
		return s.startErr
	}
	// Detached from ctx: the container outlives the test that happened to be
	// first, so that test's deadline or cancellation must not reach it.
	s.admin, s.startErr = SetupPostgresContainer(context.WithoutCancel(ctx), t)
	return s.startErr
}

// templateKey identifies the migrations directory a template was built from.
// It is absolute so that "../database/postgres/migrations" and a
// runtime.Caller-derived path to the same directory share one template instead
// of each paying for a migration run.
func templateKey(migrationsPath string) string {
	if abs, err := filepath.Abs(migrationsPath); err == nil {
		return abs
	}
	return filepath.Clean(migrationsPath)
}

// template returns the template database for migrationsPath, creating and
// migrating it on first use. Callers hold s.mu.
func (s *sharedServer) template(ctx context.Context, migrationsPath string) (string, error) {
	key := templateKey(migrationsPath)
	if tmpl, ok := s.templates[key]; ok {
		return tmpl.name, tmpl.err
	}
	if s.templates == nil {
		s.templates = map[string]templateDB{}
	}
	tmpl := templateDB{name: fmt.Sprintf("cudly_template_%d", len(s.templates)+1)}
	// Detached from ctx, like the container in start: the outcome is cached for
	// every later caller, so the first caller's cancellation or deadline must
	// not be what decides it. templateBuildTimeout still bounds a hung build.
	buildCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), templateBuildTimeout)
	defer cancel()
	tmpl.err = s.buildTemplate(buildCtx, tmpl.name, key)
	s.templates[key] = tmpl
	return tmpl.name, tmpl.err
}

// templateBuildTimeout bounds one template build. The full chain takes seconds;
// this is a backstop against a hang, generous enough for a loaded CI runner.
const templateBuildTimeout = 10 * time.Minute

// buildTemplate creates name, applies every migration under migrationsPath to
// it, and then closes it to connections.
func (s *sharedServer) buildTemplate(ctx context.Context, name, migrationsPath string) error {
	if err := s.exec(ctx, "CREATE DATABASE "+quoteIdent(name)); err != nil {
		return fmt.Errorf("create template database %s: %w", name, err)
	}
	conn, err := database.NewConnection(ctx, s.configFor(name), nil)
	if err != nil {
		return fmt.Errorf("connect to template database %s: %w", name, err)
	}
	err = migrations.RunMigrations(ctx, conn.Pool(), migrationsPath, "", "")
	conn.Close()
	if err != nil {
		return fmt.Errorf("migrations failed to apply to a fresh database: %w", err)
	}
	// Nothing may connect to the template from here on. An open connection
	// makes every later clone fail, and a write would leak into every later
	// test's starting state.
	if err := s.exec(ctx, "ALTER DATABASE "+quoteIdent(name)+" WITH IS_TEMPLATE true ALLOW_CONNECTIONS false"); err != nil {
		return fmt.Errorf("seal template database %s: %w", name, err)
	}
	return nil
}

// dropDatabase removes a per-test database. FORCE ends any connection the test
// leaked, which would otherwise block the drop and leave the database behind.
//
// It holds s.mu so TerminateSharedPostgres cannot close the admin connection
// mid-drop. With no server left there is nothing to drop: terminating the
// container took every database on it along.
func (s *sharedServer) dropDatabase(t *testing.T, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.admin == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := s.exec(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(name)+" WITH (FORCE)"); err != nil {
		t.Errorf("drop per-test database %s: %v", name, err)
	}
}

// configFor returns a copy of the shared container's config pointed at name.
// Callers hold s.mu.
func (s *sharedServer) configFor(name string) *database.Config {
	cfg := *s.admin.Config
	cfg.Database = name
	return &cfg
}

// exec runs sql on the admin connection. Callers hold s.mu.
func (s *sharedServer) exec(ctx context.Context, sql string) error {
	_, err := s.admin.DB.Exec(ctx, sql)
	return err
}

func quoteIdent(name string) string {
	return pgx.Identifier{name}.Sanitize()
}
