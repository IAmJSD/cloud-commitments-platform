//go:build integration
// +build integration

package auth

// store_postgres_db_test.go - DB-backed integration tests for the
// security-critical PostgresStore bootstrap paths (TEST-01, issue #1153).
// These run the real SQL against a migrated PostgreSQL testcontainer,
// proving the 19-column bootstrap INSERT, the NOT EXISTS guard and its
// agreement with AdminExists (active-membership semantics), and the
// insert-once guarantee under concurrent CreateAdminIfNone calls (two
// callers, and a 50-caller stress variant).

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// getAuthTestMigrationsPath returns the absolute path to the migrations directory.
func getAuthTestMigrationsPath() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "database", "postgres", "migrations")
}

// setupAuthTestDB starts a PostgreSQL container and runs all migrations
// without seeding a bootstrap admin (empty adminEmail), so tests start from
// the "fresh deployment, no admin yet" state CreateAdminIfNone exists for.
func setupAuthTestDB(t *testing.T) *database.Connection {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() {
		container.Cleanup(context.Background())
	})

	// Migrating is the thing under test here, not a precondition of it: a
	// migration that will not apply is a broken schema, and skipping on it would
	// report the whole suite green on exactly the defect it exists to catch
	// (issue #1597).
	require.NoError(t,
		migrations.RunMigrations(ctx, container.DB.Pool(), getAuthTestMigrationsPath(), "", ""),
		"migrations failed to apply to a fresh database")

	return container.DB
}

// resetUsers wipes the users table between scenarios. TRUNCATE is used
// deliberately: the trg_min_one_admin_delete row trigger (migration 000065)
// would otherwise veto deleting the last admin, and TRUNCATE does not fire
// row-level triggers. CASCADE clears dependent rows (sessions, api_keys).
func resetUsers(t *testing.T, db *database.Connection) {
	t.Helper()
	_, err := db.Exec(context.Background(), "TRUNCATE users CASCADE")
	require.NoError(t, err)
}

func newBootstrapCandidate(email string) *User {
	return &User{
		Email:        email,
		PasswordHash: "test-hash",
		Salt:         "test-salt",
		Active:       true,
	}
}

// TestIntegration_CreateAdminIfNone_Bootstrap exercises the real bootstrap
// INSERT end to end: column ordering, the Administrators-group forcing logic,
// and the NOT EXISTS guard's agreement with AdminExists.
func TestIntegration_CreateAdminIfNone_Bootstrap(t *testing.T) {
	db := setupAuthTestDB(t)
	store := NewPostgresStore(db)
	ctx := context.Background()

	t.Run("inserts first admin and forces Administrators group", func(t *testing.T) {
		resetUsers(t, db)

		user := newBootstrapCandidate("first-admin@example.com")
		created, err := store.CreateAdminIfNone(ctx, user)
		require.NoError(t, err)
		assert.True(t, created)

		// The inserted row must satisfy the membership predicate AdminExists uses.
		exists, err := store.AdminExists(ctx)
		require.NoError(t, err)
		assert.True(t, exists, "bootstrap admin must be visible to AdminExists")

		stored, err := store.GetUserByEmail(ctx, "first-admin@example.com")
		require.NoError(t, err)
		assert.Contains(t, stored.GroupIDs, DefaultAdminGroupID,
			"bootstrap admin must carry the Administrators group even though the caller did not supply it")

		count, err := store.CountGroupMembers(ctx, DefaultAdminGroupID)
		require.NoError(t, err)
		assert.Equal(t, 1, count)
	})

	t.Run("second sequential call is a no-op", func(t *testing.T) {
		resetUsers(t, db)

		created, err := store.CreateAdminIfNone(ctx, newBootstrapCandidate("admin-a@example.com"))
		require.NoError(t, err)
		require.True(t, created)

		created, err = store.CreateAdminIfNone(ctx, newBootstrapCandidate("admin-b@example.com"))
		require.NoError(t, err)
		assert.False(t, created, "an active admin already exists, so the guarded insert must not fire")

		count, err := store.CountGroupMembers(ctx, DefaultAdminGroupID)
		require.NoError(t, err)
		assert.Equal(t, 1, count, "the losing call must not have inserted a row")
	})

	t.Run("inactive admin counts as no admin, matching AdminExists", func(t *testing.T) {
		resetUsers(t, db)

		// Insert an INACTIVE Administrators-group member through the regular,
		// unguarded create path (INSERT does not fire the min-one-admin trigger).
		inactive := newBootstrapCandidate("inactive-admin@example.com")
		inactive.Active = false
		inactive.GroupIDs = []string{DefaultAdminGroupID}
		require.NoError(t, store.CreateUser(ctx, inactive))

		exists, err := store.AdminExists(ctx)
		require.NoError(t, err)
		require.False(t, exists, "inactive admin must not count as an existing admin")

		// The NOT EXISTS guard must agree: bootstrap proceeds.
		created, err := store.CreateAdminIfNone(ctx, newBootstrapCandidate("real-admin@example.com"))
		require.NoError(t, err)
		assert.True(t, created,
			"CreateAdminIfNone must treat an inactive-only admin set as 'no admin', mirroring AdminExists")

		count, err := store.CountGroupMembers(ctx, DefaultAdminGroupID)
		require.NoError(t, err)
		assert.Equal(t, 2, count, "CountGroupMembers counts members regardless of active flag")
	})

	t.Run("email collision with existing non-admin surfaces ErrEmailInUse", func(t *testing.T) {
		resetUsers(t, db)

		regular := newBootstrapCandidate("taken@example.com")
		// users_min_one_group (migration 000057) requires at least one group;
		// any seeded non-admin group satisfies it.
		regular.GroupIDs = []string{DefaultPurchaserGroupID}
		require.NoError(t, store.CreateUser(ctx, regular))

		created, err := store.CreateAdminIfNone(ctx, newBootstrapCandidate("taken@example.com"))
		assert.False(t, created)
		assert.ErrorIs(t, err, ErrEmailInUse)
	})
}

// TestIntegration_CreateAdminIfNone_ConcurrentBootstrapOnce proves the
// insert-once semantics under two concurrent CreateAdminIfNone calls: exactly
// one caller wins and exactly one admin row exists afterwards, across
// repeated barrier-synchronized attempts. Two callers is the production case
// (two operators bootstrapping at once) and keeps this the cheap fast path.
func TestIntegration_CreateAdminIfNone_ConcurrentBootstrapOnce(t *testing.T) {
	requireConcurrentBootstrapOnce(t, setupAuthTestDB(t), 2, 25)
}

// TestIntegration_CreateAdminIfNone_ConcurrentBootstrapOnce_Stress is the
// high-fanout variant (issue #64): 50 barrier-synchronized callers per
// iteration. Fifty callers queue on the test pool's 10 connections as well as
// on the bootstrap advisory lock, so a change that weakens the lock (dropping
// it, or switching to pg_try_advisory_xact_lock and ignoring the result) shows
// up as several winners in a round rather than depending on two callers
// happening to interleave, and the test shows the lock holds when every pool
// connection is taken by a contender.
//
// It takes its database from the binary's shared container
// (testhelpers.RequireMigratedDatabase, stopped by TestMain) rather than
// starting a container of its own.
func TestIntegration_CreateAdminIfNone_ConcurrentBootstrapOnce_Stress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db := testhelpers.RequireMigratedDatabase(ctx, t, getAuthTestMigrationsPath()).DB
	requireConcurrentBootstrapOnce(t, db, 50, 10)
}

// bootstrapOutcome is what each concurrent CreateAdminIfNone caller reports
// back to the test goroutine. Workers only send these; every assertion runs on
// the test goroutine.
type bootstrapOutcome struct {
	created bool
	err     error
}

// requireConcurrentBootstrapOnce runs iterations rounds of callers concurrent
// CreateAdminIfNone calls, released together by a barrier against an empty
// users table, and requires exactly one winner, no errors, and exactly one
// admin row after each round.
func requireConcurrentBootstrapOnce(t *testing.T, db *database.Connection, callers, iterations int) {
	t.Helper()

	store := NewPostgresStore(db)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	for i := 0; i < iterations; i++ {
		resetUsers(t, db)

		outcomes := runBootstrapRound(ctx, store, callers, i)

		winners := 0
		for n, r := range outcomes {
			require.NoError(t, r.err, "iteration %d, caller %d", i, n)
			if r.created {
				winners++
			}
		}
		require.Equal(t, 1, winners,
			"iteration %d: exactly one of %d concurrent bootstrap calls must win", i, callers)

		count, err := store.CountGroupMembers(ctx, DefaultAdminGroupID)
		require.NoError(t, err)
		require.Equal(t, 1, count,
			"iteration %d: exactly one admin row must exist after %d concurrent bootstrap calls", i, callers)
	}
}

// runBootstrapRound starts callers goroutines that each wait on a shared
// barrier and then call CreateAdminIfNone with a distinct email, and returns
// every caller's outcome once all of them have finished. Collecting all of
// them before asserting means no worker is still using the pool when the test
// fails and its cleanup closes the connection.
func runBootstrapRound(ctx context.Context, store *PostgresStore, callers, iteration int) []bootstrapOutcome {
	results := make(chan bootstrapOutcome, callers)
	start := make(chan struct{})

	for n := 0; n < callers; n++ {
		user := newBootstrapCandidate(fmt.Sprintf("admin-%d-%d@example.com", iteration, n))
		go func(u *User) {
			<-start
			created, err := store.CreateAdminIfNone(ctx, u)
			results <- bootstrapOutcome{created: created, err: err}
		}(user)
	}
	close(start)

	outcomes := make([]bootstrapOutcome, 0, callers)
	for n := 0; n < callers; n++ {
		outcomes = append(outcomes, <-results)
	}
	return outcomes
}
