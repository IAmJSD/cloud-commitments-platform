//go:build integration

package config

// Real PostgreSQL tests for the per-config ladder lock. Waiting is observed in
// pg_locks (this config's advisory key, granted=false), never with sleeps.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ladderLockWaiters counts sessions blocked on this config's ladder lock. The
// bigint advisory key is split across pg_locks classid (high 32 bits) and
// objid (low 32 bits).
func ladderLockWaiters(ctx context.Context, t *testing.T, store *PostgresStore, cfgID string) int {
	t.Helper()
	var n int
	require.NoError(t, store.db.QueryRow(ctx, `
		SELECT count(*) FROM pg_locks l, (SELECT hashtextextended($1, 0) AS k) h
		WHERE l.locktype = 'advisory' AND l.objsubid = 1 AND NOT l.granted
		  AND l.classid = ((h.k >> 32) & 4294967295)::oid
		  AND l.objid = (h.k & 4294967295)::oid`, ladderConfigLockKey(cfgID)).Scan(&n))
	return n
}

func waitForLadderWaiters(ctx context.Context, t *testing.T, store *PostgresStore, cfgID string, want int) {
	t.Helper()
	require.Eventually(t, func() bool { return ladderLockWaiters(ctx, t, store, cfgID) == want },
		10*time.Second, 20*time.Millisecond, "expected %d waiter(s) on the ladder lock", want)
}

// lockTestAsOf is the fixed run start shared by lockTestRun and the PlanAndSave
// calls (they must match).
var lockTestAsOf = time.Now().UTC().Truncate(time.Microsecond)

func lockTestRun(cfgID string) *LadderRunDB {
	return &LadderRunDB{ConfigID: &cfgID, StartedAt: lockTestAsOf, Status: ladder.RunStatusPlanned}
}

// holdLadderLock runs PlanAndSaveLadderRun with a build that blocks until the
// returned release func is called, so the caller owns the lock meanwhile.
func holdLadderLock(ctx context.Context, t *testing.T, store *PostgresStore, cfgID string) (release func() error) {
	t.Helper()
	entered, gate, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := store.PlanAndSaveLadderRun(ctx, cfgID, lockTestAsOf, func([]LadderInFlightRow) (*LadderRunDB, []LadderTrancheDB, error) {
			close(entered)
			<-gate
			return lockTestRun(cfgID), nil, nil
		})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("holder never entered build")
	}
	return func() error { close(gate); return <-done }
}

func countRuns(ctx context.Context, t *testing.T, store *PostgresStore, cfgID string) int {
	t.Helper()
	var n int
	require.NoError(t, store.db.QueryRow(ctx, `SELECT count(*) FROM ladder_runs WHERE config_id = $1`, cfgID).Scan(&n))
	return n
}

func TestLadderLock_ClaimWaitsForPlanner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store := setupLadderStore(ctx, t)
	cfgID := seedLadderConfigWithExtID(ctx, t, store, "711111111111")
	tr := lifecycleTranche(cfgID, 1, ladder.TrancheStatusScheduled, time.Now().UTC().Add(-time.Hour))
	basis := LadderAmountOnDemand
	tr.AmountBasis = &basis
	require.NoError(t, store.SaveLadderTranches(ctx, []LadderTrancheDB{tr}))
	release := holdLadderLock(ctx, t, store, cfgID)

	var claimEntered atomic.Bool
	inner := newExecCreator(ctx, t, store, new(atomic.Int64))
	claimDone := make(chan error, 1)
	go func() {
		_, err := store.ClaimDueLadderTranches(ctx, cfgID, time.Now().UTC(), 1,
			func(c context.Context, tx pgx.Tx, tr LadderTrancheDB) (string, error) {
				claimEntered.Store(true)
				return inner(c, tx, tr)
			})
		claimDone <- err
	}()
	waitForLadderWaiters(ctx, t, store, cfgID, 1)
	assert.False(t, claimEntered.Load(), "claim must not reach its callback while the planner holds the lock")
	require.NoError(t, release())
	require.NoError(t, <-claimDone)
	assert.True(t, claimEntered.Load(), "claim proceeds once the planner releases")
}

func TestLadderLock_IsPerConfig(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store := setupLadderStore(ctx, t)
	a := seedLadderConfigWithExtID(ctx, t, store, "721111111111")
	b := seedLadderConfigWithExtID(ctx, t, store, "722222222222")
	release := holdLadderLock(ctx, t, store, a)
	_, err := store.PlanAndSaveLadderRun(ctx, b, lockTestAsOf, func([]LadderInFlightRow) (*LadderRunDB, []LadderTrancheDB, error) {
		return lockTestRun(b), nil, nil
	})
	require.NoError(t, err, "a held lock on config A must not block config B")
	require.NoError(t, release())
}

func TestLadderLock_ReleasedOnBuildErrorPanicAndCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store := setupLadderStore(ctx, t)
	cfgID := seedLadderConfigWithExtID(ctx, t, store, "731111111111")
	ok := func([]LadderInFlightRow) (*LadderRunDB, []LadderTrancheDB, error) { //nolint:unparam // matches LadderPlanBuilder
		return lockTestRun(cfgID), nil, nil
	}
	plan := func(c context.Context, b LadderPlanBuilder) error {
		_, err := store.PlanAndSaveLadderRun(c, cfgID, lockTestAsOf, b)
		return err
	}

	t.Run("build error", func(t *testing.T) {
		err := plan(ctx, func([]LadderInFlightRow) (*LadderRunDB, []LadderTrancheDB, error) {
			return nil, nil, errors.New("boom")
		})
		require.ErrorContains(t, err, "boom")
		before := countRuns(ctx, t, store, cfgID)
		require.NoError(t, plan(ctx, ok))
		assert.Equal(t, before+1, countRuns(ctx, t, store, cfgID))
	})
	t.Run("build panic", func(t *testing.T) {
		assert.Panics(t, func() {
			_ = plan(ctx, func([]LadderInFlightRow) (*LadderRunDB, []LadderTrancheDB, error) { panic("boom") })
		})
		before := countRuns(ctx, t, store, cfgID)
		require.NoError(t, plan(ctx, ok), "the lock must be free after a panic")
		assert.Equal(t, before+1, countRuns(ctx, t, store, cfgID))
	})
	t.Run("ctx canceled while waiting", func(t *testing.T) {
		before := countRuns(ctx, t, store, cfgID)
		release := holdLadderLock(ctx, t, store, cfgID)
		waitCtx, cancelWait := context.WithCancel(ctx)
		errc := make(chan error, 1)
		go func() { errc <- plan(waitCtx, ok) }()
		waitForLadderWaiters(ctx, t, store, cfgID, 1)
		cancelWait()
		require.Error(t, <-errc)
		waitForLadderWaiters(ctx, t, store, cfgID, 0)
		require.NoError(t, release())
		assert.Equal(t, before+1, countRuns(ctx, t, store, cfgID), "only the holder's run exists; the canceled waiter wrote nothing")
		require.NoError(t, plan(ctx, ok))
	})
}

func TestLadderLock_EmptyConfigIDRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store := setupLadderStore(ctx, t)
	built := false
	_, err := store.PlanAndSaveLadderRun(ctx, "", time.Now().UTC(), func([]LadderInFlightRow) (*LadderRunDB, []LadderTrancheDB, error) {
		built = true
		return nil, nil, nil
	})
	require.Error(t, err)
	assert.False(t, built)
	_, err = store.ClaimDueLadderTranches(ctx, "", time.Now().UTC(), 1, nil)
	require.Error(t, err)
}

func TestLadderLock_RejectsNilRunAndStartedAtMismatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store := setupLadderStore(ctx, t)
	cfgID := seedLadderConfigWithExtID(ctx, t, store, "751111111111")
	_, err := store.PlanAndSaveLadderRun(ctx, cfgID, lockTestAsOf, func([]LadderInFlightRow) (*LadderRunDB, []LadderTrancheDB, error) { return nil, nil, nil })
	require.ErrorContains(t, err, "nil run")
	_, err = store.PlanAndSaveLadderRun(ctx, cfgID, lockTestAsOf.Add(time.Second), func([]LadderInFlightRow) (*LadderRunDB, []LadderTrancheDB, error) {
		return lockTestRun(cfgID), nil, nil
	})
	require.ErrorContains(t, err, "does not match asOf")
	assert.Equal(t, 0, countRuns(ctx, t, store, cfgID))
}

// The lock_timeout bounds only the advisory wait, and the transaction is READ
// COMMITTED even when the database default is REPEATABLE READ.
func TestLadderLock_TimeoutBoundsOnlyTheWaitAndIsolationIsReadCommitted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store := setupLadderStore(ctx, t)
	cfgID := seedLadderConfigWithExtID(ctx, t, store, "741111111111")

	old := ladderLockTimeout
	ladderLockTimeout = 300 * time.Millisecond
	defer func() { ladderLockTimeout = old }()

	release := holdLadderLock(ctx, t, store, cfgID)
	_, err := store.PlanAndSaveLadderRun(ctx, cfgID, lockTestAsOf, func([]LadderInFlightRow) (*LadderRunDB, []LadderTrancheDB, error) {
		return lockTestRun(cfgID), nil, nil
	})
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "a stuck holder must fail the waiter loudly")
	assert.Equal(t, "55P03", pgErr.Code)
	require.NoError(t, release())

	var dbName string
	require.NoError(t, store.db.QueryRow(ctx, `SELECT current_database()`).Scan(&dbName))
	_, err = store.db.Exec(ctx, `ALTER DATABASE `+pgx.Identifier{dbName}.Sanitize()+` SET default_transaction_isolation = 'repeatable read'`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = store.db.Exec(context.Background(), `ALTER DATABASE `+pgx.Identifier{dbName}.Sanitize()+` RESET default_transaction_isolation`)
	})
	pool := store.db.(interface{ Pool() *pgxpool.Pool }).Pool()
	pool.Reset() // new connections pick up the database default

	past := time.Now().UTC().Add(-time.Hour)
	require.NoError(t, store.SaveLadderTranches(ctx, []LadderTrancheDB{lifecycleTranche(cfgID, 1, ladder.TrancheStatusScheduled, past)}))
	inner := newExecCreator(ctx, t, store, new(atomic.Int64))
	var lockTimeout, isolation string
	_, err = store.ClaimDueLadderTranches(ctx, cfgID, time.Now().UTC(), 1, func(c context.Context, tx pgx.Tx, tr LadderTrancheDB) (string, error) {
		require.NoError(t, tx.QueryRow(c, `SHOW lock_timeout`).Scan(&lockTimeout))
		require.NoError(t, tx.QueryRow(c, `SHOW transaction_isolation`).Scan(&isolation))
		return inner(c, tx, tr)
	})
	require.NoError(t, err)
	assert.Equal(t, "0", lockTimeout, "the wait timeout must not leak into later statements")
	assert.Equal(t, "read committed", isolation)
}
