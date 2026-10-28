//go:build integration

package config

// Real PostgreSQL tests for the ladder tranche lifecycle (issue #118, PR-A).
// Nothing here uses mocks. Store methods run against a real database with
// migrations applied; goroutines race for real row locks.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lifecycleTranche(cfgID string, amount float64, status ladder.TrancheStatus, at time.Time) LadderTrancheDB {
	tr := LadderTrancheDB{
		ID: uuid.New().String(), ConfigID: &cfgID, LayerType: ladder.LayerComputeSP,
		Term: ladder.Term1Year, PaymentOption: ladder.PaymentNoUpfront,
		Status: status, AmountUSDHr: amount, ScheduledDate: at,
	}
	if status == ladder.TrancheStatusCompleted {
		settled := at
		tr.SettledAt = &settled
	}
	return tr
}

// claimableTranche is a scheduled lifecycleTranche planned in commitment
// dollars, the only basis ClaimDueLadderTranches fires (#946). Rate 1 keeps
// its on-demand-equivalent netting equal to its amount.
func claimableTranche(cfgID string, amount float64, at time.Time) LadderTrancheDB {
	tr := lifecycleTranche(cfgID, amount, ladder.TrancheStatusScheduled, at)
	basis, rate := LadderAmountCommitment, "1"
	tr.AmountBasis, tr.CommitmentRate = &basis, &rate
	return tr
}

func trancheStatus(ctx context.Context, t *testing.T, store *PostgresStore, id string) ladder.TrancheStatus {
	t.Helper()
	var st string
	require.NoError(t, store.db.QueryRow(ctx, `SELECT status FROM ladder_tranches WHERE id = $1`, id).Scan(&st))
	return ladder.TrancheStatus(st)
}

// newExecCreator returns a claim callback that inserts a real
// purchase_executions row in the claim transaction and counts invocations.
func newExecCreator(ctx context.Context, t *testing.T, store *PostgresStore, calls *atomic.Int64) func(context.Context, pgx.Tx, LadderTrancheDB) (string, error) {
	t.Helper()
	var planID string
	require.NoError(t, store.db.QueryRow(ctx, `INSERT INTO purchase_plans (name) VALUES ('ladder-118') RETURNING id`).Scan(&planID))
	return func(ctx context.Context, tx pgx.Tx, _ LadderTrancheDB) (string, error) {
		calls.Add(1)
		var execID string
		err := tx.QueryRow(ctx, `INSERT INTO purchase_executions (plan_id, scheduled_date) VALUES ($1, NOW()) RETURNING execution_id`, planID).Scan(&execID)
		return execID, err
	}
}

func inFlight(ctx context.Context, t *testing.T, store *PostgresStore, cfgID string, asOf time.Time) float64 {
	t.Helper()
	rows, err := inFlightLadderRows(ctx, store.db, true, cfgID, asOf)
	require.NoError(t, err)
	return mustOnDemand(t, rows)
}

// Property: for every status x scheduled_date offset around the grace
// boundary, a scheduled row is counted by netting XOR canceled by the sweep
// (never neither, never both), and non-scheduled rows are never touched by the
// sweep. Amounts are distinct powers of two so the netted sum identifies
// exactly which rows were counted.
func TestLadderGraceBoundary_ScheduledRowsAreCountedOrCancelledNeverNeither(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	store := setupLadderStore(ctx, t)
	cfgID := seedLadderConfigWithExtID(ctx, t, store, "611111111111")
	asOf := time.Now().UTC().Truncate(time.Microsecond)
	offsets := []time.Duration{
		-48 * time.Hour, -LadderFireGrace - time.Microsecond, -LadderFireGrace,
		-LadderFireGrace + time.Microsecond, -time.Hour, 0, time.Hour,
	}

	type row struct {
		tr      LadderTrancheDB
		bit     float64
		counted bool
		sched   bool
	}
	rows := make([]row, 0, len(offsets)*len(allLadderTrancheStatuses))
	all := make([]LadderTrancheDB, 0, cap(rows))
	bit := 1.0
	for _, st := range allLadderTrancheStatuses {
		for _, off := range offsets {
			tr := lifecycleTranche(cfgID, bit, st, asOf.Add(off))
			counted := st == ladder.TrancheStatusFired ||
				(st == ladder.TrancheStatusScheduled && off > -LadderFireGrace)
			rows = append(rows, row{tr: tr, bit: bit, counted: counted, sched: st == ladder.TrancheStatusScheduled})
			all = append(all, tr)
			bit *= 2
		}
	}
	require.NoError(t, store.SaveLadderTranches(ctx, all))

	var want float64
	for _, r := range rows {
		if r.counted {
			want += r.bit
		}
	}
	assert.Equal(t, want, inFlight(ctx, t, store, cfgID, asOf), "netting must count exactly fired + fireable scheduled")

	_, err := store.SaveLadderRunWithTranches(ctx,
		&LadderRunDB{ConfigID: &cfgID, StartedAt: asOf, Status: ladder.RunStatusPlanned}, nil)
	require.NoError(t, err)

	assert.Equal(t, want, inFlight(ctx, t, store, cfgID, asOf), "the sweep must cancel only rows netting did not count")
	for _, r := range rows {
		got := trancheStatus(ctx, t, store, r.tr.ID)
		if r.sched {
			canceled := got == ladder.TrancheStatusCancelled
			assert.NotEqual(t, r.counted, canceled, "scheduled row at %v: counted=%v status=%s (must be counted XOR canceled)",
				r.tr.ScheduledDate.Sub(asOf), r.counted, got)
		} else {
			assert.Equal(t, r.tr.Status, got, "sweep must not touch a %s row", r.tr.Status)
		}
	}
}

func TestClaimDueLadderTranches_ClaimsOnlyDueUnexpiredAndKeepsNetting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	store := setupLadderStore(ctx, t)
	cfgID := seedLadderConfigWithExtID(ctx, t, store, "622222222222")
	otherID := seedLadderConfigWithExtID(ctx, t, store, "633333333333")
	asOf := time.Now().UTC().Truncate(time.Microsecond)
	var calls atomic.Int64
	create := newExecCreator(ctx, t, store, &calls)

	due := claimableTranche(cfgID, 1, asOf.Add(-time.Hour))
	dueNow := claimableTranche(cfgID, 2, asOf)
	future := claimableTranche(cfgID, 4, asOf.Add(time.Microsecond))
	expired := claimableTranche(cfgID, 8, asOf.Add(-LadderFireGrace))
	otherCfg := claimableTranche(otherID, 16, asOf.Add(-time.Hour))
	require.NoError(t, store.SaveLadderTranches(ctx, []LadderTrancheDB{due, dueNow, future, expired, otherCfg}))
	before := inFlight(ctx, t, store, cfgID, asOf)
	assert.Equal(t, 1.0+2+4, before, "expired row is not netted")

	claimed, err := store.ClaimDueLadderTranches(ctx, cfgID, asOf, 10, create)
	require.NoError(t, err)
	require.Len(t, claimed, 2)
	assert.Equal(t, due.ID, claimed[0].ID, "oldest due first")
	assert.Equal(t, dueNow.ID, claimed[1].ID)
	for _, c := range claimed {
		require.NotNil(t, c.ExecutionID)
		assert.Equal(t, ladder.TrancheStatusFired, c.Status)
		assert.Equal(t, ladder.TrancheStatusFired, trancheStatus(ctx, t, store, c.ID))
	}
	assert.Equal(t, int64(2), calls.Load())
	assert.Equal(t, ladder.TrancheStatusScheduled, trancheStatus(ctx, t, store, future.ID), "not yet due")
	assert.Equal(t, ladder.TrancheStatusScheduled, trancheStatus(ctx, t, store, expired.ID), "past grace: never claimed")
	assert.Equal(t, ladder.TrancheStatusScheduled, trancheStatus(ctx, t, store, otherCfg.ID), "other config untouched")
	assert.Equal(t, before, inFlight(ctx, t, store, cfgID, asOf), "claiming moves scheduled -> fired without changing netting")

	again, err := store.ClaimDueLadderTranches(ctx, cfgID, asOf, 10, create)
	require.NoError(t, err)
	assert.Empty(t, again, "a second claim finds nothing: idempotent")

	limited := claimableTranche(cfgID, 32, asOf.Add(-time.Minute))
	limited2 := claimableTranche(cfgID, 64, asOf.Add(-2*time.Minute))
	require.NoError(t, store.SaveLadderTranches(ctx, []LadderTrancheDB{limited, limited2}))
	one, err := store.ClaimDueLadderTranches(ctx, cfgID, asOf, 1, create)
	require.NoError(t, err)
	require.Len(t, one, 1)
	assert.Equal(t, limited2.ID, one[0].ID)

	_, err = store.ClaimDueLadderTranches(ctx, cfgID, asOf, 0, create)
	require.Error(t, err)
}

// Legacy rows (NULL basis from before migration 000121, or
// on_demand_equivalent) hold on-demand-equivalent dollars, but an executor buys
// amount_usd_hr as commitment dollars, so firing one would over-buy by 1/rate.
// They are never claimed, even ahead of a due commitment row under a limit;
// they stay netted while within grace, and the overdue sweep cancels them
// after it, so the next run re-plans their gap in commitment dollars (#946).
func TestClaimDueLadderTranches_NeverClaimsOnDemandOrUnknownBasisRows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	store := setupLadderStore(ctx, t)
	cfgID := seedLadderConfigWithExtID(ctx, t, store, "688888888888")
	asOf := time.Now().UTC().Truncate(time.Microsecond)
	var calls atomic.Int64
	create := newExecCreator(ctx, t, store, &calls)

	unknown := lifecycleTranche(cfgID, 1, ladder.TrancheStatusScheduled, asOf.Add(-3*time.Hour))
	od := lifecycleTranche(cfgID, 2, ladder.TrancheStatusScheduled, asOf.Add(-2*time.Hour))
	odBasis := LadderAmountOnDemand
	od.AmountBasis = &odBasis
	native := claimableTranche(cfgID, 4, asOf.Add(-time.Hour))
	half := "1/2"
	native.CommitmentRate = &half
	require.NoError(t, store.SaveLadderTranches(ctx, []LadderTrancheDB{unknown, od, native}))
	assert.Equal(t, 1.0+2+8, inFlight(ctx, t, store, cfgID, asOf), "4 commitment at rate 1/2 nets as 8 on-demand-equivalent")

	claimed, err := store.ClaimDueLadderTranches(ctx, cfgID, asOf, 1, create)
	require.NoError(t, err)
	require.Len(t, claimed, 1, "the older legacy rows must not use up the limit")
	assert.Equal(t, native.ID, claimed[0].ID)
	again, err := store.ClaimDueLadderTranches(ctx, cfgID, asOf, 10, create)
	require.NoError(t, err)
	assert.Empty(t, again, "legacy rows are never claimable")
	assert.Equal(t, int64(1), calls.Load(), "no execution row for a legacy tranche")
	assert.Equal(t, ladder.TrancheStatusScheduled, trancheStatus(ctx, t, store, unknown.ID))
	assert.Equal(t, ladder.TrancheStatusScheduled, trancheStatus(ctx, t, store, od.ID))
	assert.Equal(t, 1.0+2+8, inFlight(ctx, t, store, cfgID, asOf), "legacy rows stay netted within grace")

	later := asOf.Add(LadderFireGrace)
	_, err = store.SaveLadderRunWithTranches(ctx, &LadderRunDB{ConfigID: &cfgID, StartedAt: later, Status: ladder.RunStatusPlanned}, nil)
	require.NoError(t, err)
	assert.Equal(t, ladder.TrancheStatusCancelled, trancheStatus(ctx, t, store, unknown.ID), "past grace the sweep cancels it")
	assert.Equal(t, ladder.TrancheStatusCancelled, trancheStatus(ctx, t, store, od.ID), "past grace the sweep cancels it")
}

func TestClaimDueLadderTranches_CallbackFailureRollsBackClaimAndExecution(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	store := setupLadderStore(ctx, t)
	cfgID := seedLadderConfigWithExtID(ctx, t, store, "644444444444")
	asOf := time.Now().UTC().Truncate(time.Microsecond)
	var calls atomic.Int64
	create := newExecCreator(ctx, t, store, &calls)
	first := claimableTranche(cfgID, 1, asOf.Add(-2*time.Hour))
	second := claimableTranche(cfgID, 2, asOf.Add(-time.Hour))
	require.NoError(t, store.SaveLadderTranches(ctx, []LadderTrancheDB{first, second}))
	var execsBefore int
	require.NoError(t, store.db.QueryRow(ctx, `SELECT count(*) FROM purchase_executions`).Scan(&execsBefore))

	boom := errors.New("create failed")
	failSecond := func(ctx context.Context, tx pgx.Tx, tr LadderTrancheDB) (string, error) {
		if tr.ID == second.ID {
			return "", boom
		}
		return create(ctx, tx, tr)
	}
	claimed, err := store.ClaimDueLadderTranches(ctx, cfgID, asOf, 10, failSecond)
	require.ErrorIs(t, err, boom)
	assert.Nil(t, claimed)
	assert.Equal(t, ladder.TrancheStatusScheduled, trancheStatus(ctx, t, store, first.ID), "first claim must roll back with the failed second")
	var execsAfter int
	require.NoError(t, store.db.QueryRow(ctx, `SELECT count(*) FROM purchase_executions`).Scan(&execsAfter))
	assert.Equal(t, execsBefore, execsAfter, "no execution row may survive a rolled-back claim")

	_, err = store.ClaimDueLadderTranches(ctx, cfgID, asOf, 10,
		func(context.Context, pgx.Tx, LadderTrancheDB) (string, error) { return "", nil })
	require.Error(t, err, "an empty execution id must fail loud")
	assert.Equal(t, ladder.TrancheStatusScheduled, trancheStatus(ctx, t, store, first.ID))
}

func TestTransitionLadderTranche_EveryLegalEdgeAndCompareAndSet(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	store := setupLadderStore(ctx, t)
	cfgID := seedLadderConfigWithExtID(ctx, t, store, "655555555555")
	asOf := time.Now().UTC().Truncate(time.Microsecond)
	var calls atomic.Int64
	create := newExecCreator(ctx, t, store, &calls)

	// scheduled -> canceled, once.
	canceled := claimableTranche(cfgID, 1, asOf.Add(time.Hour))
	// scheduled -> fired (claim) -> completed / failed.
	toCompleted := claimableTranche(cfgID, 2, asOf.Add(-time.Hour))
	toFailed := claimableTranche(cfgID, 4, asOf.Add(-time.Hour))
	require.NoError(t, store.SaveLadderTranches(ctx, []LadderTrancheDB{canceled, toCompleted, toFailed}))
	require.Equal(t, 7.0, inFlight(ctx, t, store, cfgID, asOf))

	ok, err := store.TransitionLadderTranche(ctx, canceled.ID, ladder.TrancheStatusScheduled, ladder.TrancheStatusCancelled, "test")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = store.TransitionLadderTranche(ctx, canceled.ID, ladder.TrancheStatusScheduled, ladder.TrancheStatusCancelled, "test")
	require.NoError(t, err)
	assert.False(t, ok, "second apply loses the compare-and-set")
	assert.Equal(t, 6.0, inFlight(ctx, t, store, cfgID, asOf), "canceled releases netting")

	_, err = store.ClaimDueLadderTranches(ctx, cfgID, asOf, 10, create)
	require.NoError(t, err)
	assert.Equal(t, 6.0, inFlight(ctx, t, store, cfgID, asOf), "fired still nets")

	ok, err = store.TransitionLadderTranche(ctx, toCompleted.ID, ladder.TrancheStatusScheduled, ladder.TrancheStatusCancelled, "test")
	require.NoError(t, err)
	assert.False(t, ok, "a claimed tranche can no longer be canceled from scheduled")
	ok, err = store.TransitionLadderTranche(ctx, toCompleted.ID, ladder.TrancheStatusFired, ladder.TrancheStatusCompleted, "test")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, 4.0, inFlight(ctx, t, store, cfgID, asOf), "completed releases netting (now counted in E)")
	ok, err = store.TransitionLadderTranche(ctx, toFailed.ID, ladder.TrancheStatusFired, ladder.TrancheStatusFailed, "test")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, 0.0, inFlight(ctx, t, store, cfgID, asOf), "failed releases netting")

	for _, id := range []string{canceled.ID, toCompleted.ID, toFailed.ID} {
		for _, to := range allLadderTrancheStatuses {
			st := trancheStatus(ctx, t, store, id)
			ok, err = store.TransitionLadderTranche(ctx, id, st, to, "test")
			assert.Error(t, err, "terminal %s -> %s", st, to)
			assert.False(t, ok)
		}
	}

	ok, err = store.TransitionLadderTranche(ctx, uuid.New().String(), ladder.TrancheStatusFired, ladder.TrancheStatusFailed, "test")
	require.NoError(t, err)
	assert.False(t, ok, "unknown id is a lost race, not an error")
}

func TestClaimDueLadderTranches_ConcurrentClaimersExactlyOneWinner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	store := setupLadderStore(ctx, t)
	cfgID := seedLadderConfigWithExtID(ctx, t, store, "666666666666")
	asOf := time.Now().UTC().Truncate(time.Microsecond)
	var calls atomic.Int64
	create := newExecCreator(ctx, t, store, &calls)
	tr := claimableTranche(cfgID, 1, asOf.Add(-time.Hour))
	require.NoError(t, store.SaveLadderTranches(ctx, []LadderTrancheDB{tr}))

	const n = 8
	var wg sync.WaitGroup
	var winners atomic.Int64
	start := make(chan struct{})
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := store.ClaimDueLadderTranches(ctx, cfgID, asOf, 10, create)
			if err != nil {
				errs <- err
				return
			}
			winners.Add(int64(len(got)))
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, int64(1), winners.Load(), "exactly one claimer wins the tranche")
	assert.Equal(t, int64(1), calls.Load(), "exactly one execution row is created")
	assert.Equal(t, ladder.TrancheStatusFired, trancheStatus(ctx, t, store, tr.ID))
}

// Claim vs cancel on the same row: whichever wins, the loser must observe it,
// and the final state must match the winner. Repeated to shake out ordering.
func TestClaimVsCancelAndOverdueSweep_ExactlyOneOutcome(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	store := setupLadderStore(ctx, t)
	cfgID := seedLadderConfigWithExtID(ctx, t, store, "677777777777")
	asOf := time.Now().UTC().Truncate(time.Microsecond)
	var calls atomic.Int64
	create := newExecCreator(ctx, t, store, &calls)

	for i := 0; i < 25; i++ {
		tr := claimableTranche(cfgID, 1, asOf.Add(-time.Hour))
		require.NoError(t, store.SaveLadderTranches(ctx, []LadderTrancheDB{tr}))
		var claimWon, cancelWon bool
		var claimErr, cancelErr, sweepErr error
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			var got []LadderTrancheDB
			// Scope the claim to this tranche by limiting to the single oldest row.
			got, claimErr = store.ClaimDueLadderTranches(ctx, cfgID, asOf, 1, create)
			for _, g := range got {
				claimWon = claimWon || g.ID == tr.ID
			}
		}()
		go func() {
			defer wg.Done()
			cancelWon, cancelErr = store.TransitionLadderTranche(ctx, tr.ID, ladder.TrancheStatusScheduled, ladder.TrancheStatusCancelled, "test")
		}()
		go func() {
			defer wg.Done()
			// The sweep cancels only rows past grace, so a within-grace row must survive it.
			_, sweepErr = store.SaveLadderRunWithTranches(ctx,
				&LadderRunDB{ConfigID: &cfgID, StartedAt: asOf, Status: ladder.RunStatusPlanned}, nil)
		}()
		wg.Wait()
		require.NoError(t, claimErr)
		require.NoError(t, cancelErr)
		require.NoError(t, sweepErr)
		final := trancheStatus(ctx, t, store, tr.ID)
		switch final {
		case ladder.TrancheStatusFired:
			assert.True(t, claimWon && !cancelWon, "iteration %d: fired but claimWon=%v cancelWon=%v", i, claimWon, cancelWon)
		case ladder.TrancheStatusCancelled:
			assert.True(t, cancelWon && !claimWon, "iteration %d: canceled but claimWon=%v cancelWon=%v", i, claimWon, cancelWon)
		default:
			t.Fatalf("iteration %d: unexpected final status %s", i, final)
		}
		// Retire the row so the next iteration's limit-1 claim sees only its own tranche.
		_, err := store.db.Exec(ctx, `UPDATE ladder_tranches SET status = 'completed', settled_at = now() WHERE id = $1`, tr.ID)
		require.NoError(t, err)
	}
}

// A transition stores its reason; the overdue sweep stamps 'expired'.
func TestLadderTranche_StatusReasonIsStored(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	cfgID := seedLadderConfig(ctx, t, store)
	now := time.Now().UTC().Truncate(time.Microsecond)
	reasonOf := func(id string) *string {
		var r *string
		require.NoError(t, store.db.QueryRow(ctx, `SELECT status_reason FROM ladder_tranches WHERE id = $1`, id).Scan(&r))
		return r
	}

	fired := lifecycleTranche(cfgID, 1, ladder.TrancheStatusFired, now)
	overdue := lifecycleTranche(cfgID, 1, ladder.TrancheStatusScheduled, now.Add(-LadderFireGrace-time.Hour))
	require.NoError(t, store.SaveLadderTranches(ctx, []LadderTrancheDB{fired, overdue}))

	_, err := store.TransitionLadderTranche(ctx, fired.ID, ladder.TrancheStatusFired, ladder.TrancheStatusFailed, "")
	require.Error(t, err, "a failed transition needs a reason")
	ok, err := store.TransitionLadderTranche(ctx, fired.ID, ladder.TrancheStatusFired, ladder.TrancheStatusFailed, "no_submission")
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, reasonOf(fired.ID))
	assert.Equal(t, "no_submission", *reasonOf(fired.ID))

	require.NoError(t, store.WithTx(ctx, func(tx pgx.Tx) error {
		return cancelOverdueLadderTranchesTx(ctx, tx, &cfgID, now)
	}))
	require.NotNil(t, reasonOf(overdue.ID))
	assert.Equal(t, "expired", *reasonOf(overdue.ID))
}
