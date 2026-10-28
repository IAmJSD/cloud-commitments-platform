//go:build integration

package auth

// store_postgres_admin_lock_db_test.go - real-PostgreSQL tests for the
// admin-invariant advisory lock ordering (issue #63). Every write that can
// lower the active-admin count must take adminInvariantAdvisoryLockKey as the
// first statement of its transaction, before any users row lock, the same as
// CreateAdminIfNone. The min-one-admin deferred trigger (migration 000065)
// takes the same key again at commit, which is then a no-op re-acquire.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
)

// adminDemotingWrite is one store-level write that can lower the active-admin
// count. target is the row as the caller read it.
type adminDemotingWrite struct {
	name string
	run  func(ctx context.Context, store *PostgresStore, target *User) error
}

func adminDemotingWrites() []adminDemotingWrite {
	return []adminDemotingWrite{
		{"UpdateUserAdminFields deactivate", func(ctx context.Context, store *PostgresStore, target *User) error {
			next := *target
			now := time.Now()
			next.Active, next.DeactivatedAt = false, &now
			return store.UpdateUserAdminFields(ctx, &next, target.Email, target.GroupIDs, target.Active)
		}},
		{"UpdateUserAdminFields drop Administrators group", func(ctx context.Context, store *PostgresStore, target *User) error {
			next := *target
			next.GroupIDs = []string{DefaultPurchaserGroupID}
			return store.UpdateUserAdminFields(ctx, &next, target.Email, target.GroupIDs, target.Active)
		}},
		{"DeleteUser", func(ctx context.Context, store *PostgresStore, target *User) error {
			return store.DeleteUser(ctx, target.ID)
		}},
	}
}

// seedActiveAdmins inserts n active Administrators-group members. CreateUser
// is a plain INSERT, which the min-one-admin triggers do not watch.
func seedActiveAdmins(t *testing.T, store *PostgresStore, prefix string, n int) []*User {
	t.Helper()
	admins := make([]*User, n)
	for i := range admins {
		u := newBootstrapCandidate(fmt.Sprintf("%s-%d@example.com", prefix, i))
		u.GroupIDs = []string{DefaultAdminGroupID}
		require.NoError(t, store.CreateUser(context.Background(), u))
		admins[i] = u
	}
	return admins
}

func countActiveAdmins(t *testing.T, db DBConnection) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM users WHERE `+adminGroupContainsClause+` AND active = true`,
		DefaultAdminGroupID).Scan(&n))
	return n
}

// waitForAdminLockWaiter blocks until some backend is queued on the
// admin-invariant advisory lock. A bigint advisory key is reported in
// pg_locks split into classid (high 32 bits) and objid (low 32 bits).
func waitForAdminLockWaiter(t *testing.T, db DBConnection) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		require.NoError(t, db.QueryRow(context.Background(), `
			SELECT COUNT(*) FROM pg_locks
			WHERE locktype = 'advisory' AND NOT granted
			  AND ((classid::bigint << 32) | objid::bigint) = $1`,
			int64(adminInvariantAdvisoryLockKey)).Scan(&waiting))
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no backend queued on the admin-invariant advisory lock")
}

// TestIntegration_AdminDemotingWrites_LockBeforeRowLock pins the lock order.
// A test transaction holds the admin-invariant lock, as a bootstrap would,
// while each write runs. The write must queue on that lock before touching
// the target row, so the holder can still lock the row with NOWAIT. Before
// issue #63 the write updated or deleted the row first and only reached the
// lock in the deferred trigger at commit, so NOWAIT failed with 55P03.
func TestIntegration_AdminDemotingWrites_LockBeforeRowLock(t *testing.T) {
	db := setupAuthTestDB(t)
	store := NewPostgresStore(db)

	for _, write := range adminDemotingWrites() {
		t.Run(write.name, func(t *testing.T) {
			resetUsers(t, db)
			// Two admins, so the write is allowed once the holder lets go.
			target := seedActiveAdmins(t, store, "lock-order", 2)[0]
			ctx := context.Background()

			holder, err := db.Pool().Begin(ctx)
			require.NoError(t, err)
			defer func() { _ = holder.Rollback(ctx) }()
			_, err = holder.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", adminInvariantAdvisoryLockKey)
			require.NoError(t, err)

			done := make(chan error, 1)
			go func() { done <- write.run(ctx, store, target) }()
			waitForAdminLockWaiter(t, db)

			_, err = holder.Exec(ctx, "SELECT 1 FROM users WHERE id = $1 FOR UPDATE NOWAIT", target.ID)
			require.NoError(t, err,
				"the write locked the users row before the admin-invariant advisory lock")
			require.NoError(t, holder.Commit(ctx))

			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(30 * time.Second):
				t.Fatal("write did not finish after the advisory lock was released")
			}
			assert.Equal(t, 1, countActiveAdmins(t, db))
		})
	}
}

// serviceDemotion is one service-level way of taking admin standing away.
type serviceDemotion struct {
	name string
	run  func(ctx context.Context, svc *Service, userID string) error
}

func serviceDemotions() []serviceDemotion {
	inactive := false
	return []serviceDemotion{
		{"deactivate", func(ctx context.Context, svc *Service, id string) error {
			_, err := svc.UpdateUser(ctx, "", id, UpdateUserRequest{Active: &inactive})
			return err
		}},
		{"drop group", func(ctx context.Context, svc *Service, id string) error {
			_, err := svc.UpdateUser(ctx, "", id, UpdateUserRequest{GroupIDs: []string{DefaultPurchaserGroupID}})
			return err
		}},
		{"delete", func(ctx context.Context, svc *Service, id string) error {
			return svc.DeleteUser(ctx, id)
		}},
	}
}

// runConcurrently starts every fn at once behind a barrier and returns their
// errors in order.
func runConcurrently(fns ...func() error) []error {
	errs := make([]error, len(fns))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, fn := range fns {
		wg.Add(1)
		go func(i int, fn func() error) {
			defer wg.Done()
			<-start
			errs[i] = fn()
		}(i, fn)
	}
	close(start)
	wg.Wait()
	return errs
}

// TestIntegration_ConcurrentDemotionOfLastTwoAdmins races every pair of
// demote, deactivate and delete against the two remaining admins: exactly one
// of the pair succeeds, the other gets ErrLastAdmin, and one active admin is
// left.
func TestIntegration_ConcurrentDemotionOfLastTwoAdmins(t *testing.T) {
	db := setupAuthTestDB(t)
	store := NewPostgresStore(db)
	svc := NewService(ServiceConfig{Store: store})
	ctx := context.Background()
	const iterations = 15

	for _, first := range serviceDemotions() {
		for _, second := range serviceDemotions() {
			t.Run(first.name+" vs "+second.name, func(t *testing.T) {
				for i := 0; i < iterations; i++ {
					resetUsers(t, db)
					admins := seedActiveAdmins(t, store, fmt.Sprintf("last-two-%d", i), 2)

					errs := runConcurrently(
						func() error { return first.run(ctx, svc, admins[0].ID) },
						func() error { return second.run(ctx, svc, admins[1].ID) },
					)

					succeeded := 0
					for _, err := range errs {
						if err == nil {
							succeeded++
							continue
						}
						require.ErrorIs(t, err, ErrLastAdmin, "iteration %d", i)
					}
					require.Equal(t, 1, succeeded, "iteration %d: exactly one demotion must win", i)
					require.Equal(t, 1, countActiveAdmins(t, db), "iteration %d", i)
				}
			})
		}
	}
}

// TestIntegration_ConcurrentBootstrapVersusDemotion races CreateAdminIfNone
// against demoting an existing admin. An admin exists throughout, so the
// bootstrap must never insert; the demotion succeeds only when another admin
// remains.
func TestIntegration_ConcurrentBootstrapVersusDemotion(t *testing.T) {
	db := setupAuthTestDB(t)
	store := NewPostgresStore(db)
	svc := NewService(ServiceConfig{Store: store})
	ctx := context.Background()
	const iterations = 15

	for _, admins := range []int{1, 2} {
		for _, demotion := range serviceDemotions() {
			t.Run(fmt.Sprintf("%d admins, %s", admins, demotion.name), func(t *testing.T) {
				for i := 0; i < iterations; i++ {
					resetUsers(t, db)
					seeded := seedActiveAdmins(t, store, fmt.Sprintf("bootstrap-race-%d", i), admins)

					var created bool
					errs := runConcurrently(
						func() error {
							var err error
							created, err = store.CreateAdminIfNone(ctx, newBootstrapCandidate(fmt.Sprintf("bootstrap-%d@example.com", i)))
							return err
						},
						func() error { return demotion.run(ctx, svc, seeded[0].ID) },
					)

					require.NoError(t, errs[0], "iteration %d", i)
					assert.False(t, created, "iteration %d: an admin existed, the bootstrap must not insert", i)
					if admins == 1 {
						require.ErrorIs(t, errs[1], ErrLastAdmin, "iteration %d", i)
					} else {
						require.NoError(t, errs[1], "iteration %d", i)
					}
					require.Equal(t, 1, countActiveAdmins(t, db), "iteration %d", i)
				}
			})
		}
	}
}

// lockTimeoutPool opens a second pool on the test database whose sessions
// give up on any lock wait after lockTimeout, so a hang fails fast instead of
// stalling the suite.
func lockTimeoutPool(t *testing.T, db *database.Connection, lockTimeout string) *pgxpool.Pool {
	t.Helper()
	cfg := db.Pool().Config().Copy()
	cfg.MaxConns = 16
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = lockTimeout
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// expectedContentionError reports whether err is an outcome the mixed
// workload can legitimately produce: the min-one-admin trigger refusing the
// last admin, a compare-and-set losing to another write, or the row having
// been deleted by another write.
func expectedContentionError(err error) bool {
	if err == nil || isLastAdminConstraintViolation(err) || errors.Is(err, ErrUserChanged) {
		return true
	}
	var pgErr *pgconn.PgError
	return !errors.As(err, &pgErr) && strings.HasPrefix(err.Error(), "user not found: ")
}

// mixedAdminWorkload returns one round of concurrent writes against three
// admins: every admin-demoting store path, plus the bootstrap and the login
// paths that lock users rows without the advisory lock.
func mixedAdminWorkload(ctx context.Context, store *PostgresStore, admins []*User) []func() error {
	var fns []func() error
	for i, write := range adminDemotingWrites() {
		target := admins[i%len(admins)]
		other := admins[(i+1)%len(admins)]
		fns = append(fns,
			func() error { return write.run(ctx, store, target) },
			func() error { return write.run(ctx, store, other) },
		)
	}
	for _, a := range admins {
		id := a.ID
		fns = append(fns,
			func() error { return store.RecordSuccessfulLogin(ctx, id) },
			func() error { return store.RecordFailedLogin(ctx, id) },
		)
	}
	fns = append(fns, func() error {
		_, err := store.CreateAdminIfNone(ctx, newBootstrapCandidate("mixed-bootstrap@example.com"))
		return err
	})
	return fns
}

// TestIntegration_AdminWrites_NoDeadlock runs many rounds of the mixed
// workload with a lock_timeout and asserts no transaction ever deadlocks
// (40P01) or times out on a lock (55P03), and that an active admin always
// remains. A write path that still locked the row before the advisory lock
// would deadlock against one that takes the advisory lock first.
func TestIntegration_AdminWrites_NoDeadlock(t *testing.T) {
	db := setupAuthTestDB(t)
	pool := lockTimeoutPool(t, db, "10s")
	store := NewPostgresStore(pool)
	ctx := context.Background()
	const rounds = 60

	for round := 0; round < rounds; round++ {
		resetUsers(t, db)
		admins := seedActiveAdmins(t, store, fmt.Sprintf("mixed-%d", round), 3)

		for _, err := range runConcurrently(mixedAdminWorkload(ctx, store, admins)...) {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) {
				require.NotEqual(t, "40P01", pgErr.Code, "round %d: deadlock: %v", round, err)
				require.NotEqual(t, "55P03", pgErr.Code, "round %d: lock wait timed out: %v", round, err)
			}
			require.True(t, expectedContentionError(err), "round %d: unexpected error: %v", round, err)
		}
		require.GreaterOrEqual(t, countActiveAdmins(t, pool), 1, "round %d", round)
	}
}
