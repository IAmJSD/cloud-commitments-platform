//go:build integration

package config

// Real PostgreSQL tests for the config-wide amend ceiling (#842 PR-B).

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

type ceilingFixture struct {
	ctx       context.Context
	t         *testing.T
	store     *PostgresStore
	cfgID     string
	accountID string
	runs      int
}

func newCeilingFixture(t *testing.T) *ceilingFixture {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	store := setupLadderStore(ctx, t)
	f := &ceilingFixture{ctx: ctx, t: t, store: store, cfgID: seedLadderConfig(ctx, t, store)}
	require.NoError(t, store.db.QueryRow(ctx, "SELECT cloud_account_id FROM ladder_configs WHERE id=$1", f.cfgID).Scan(&f.accountID))
	return f
}

func fptr(v float64) *float64 { return &v }

// plan persists a planned run through PlanAndSaveLadderRun with the given
// snapshot (nil leaves the column NULL) and one scheduled tranche per amount.
// Runs start one minute apart, oldest first, all in the past.
func (f *ceilingFixture) plan(target, existing, baseline *float64, amounts ...float64) (string, []string) {
	f.t.Helper()
	f.runs++
	asOf := time.Now().UTC().Add(-time.Duration(10-f.runs) * time.Minute).Truncate(time.Microsecond)
	runID := uuid.NewString()
	var ids []string
	_, err := f.store.PlanAndSaveLadderRun(f.ctx, f.cfgID, asOf, func(float64) (*LadderRunDB, []LadderTrancheDB, error) {
		trs := make([]LadderTrancheDB, len(amounts))
		for i, a := range amounts {
			trs[i] = lifecycleTranche(f.cfgID, a, ladder.TrancheStatusScheduled, time.Now().UTC().Add(time.Hour))
			trs[i].RunID = &runID
			ids = append(ids, trs[i].ID)
		}
		return &LadderRunDB{ID: runID, ConfigID: &f.cfgID, StartedAt: asOf, Status: ladder.RunStatusPlanned, TotalHourlyCommit: 100, // the original_total term is not under test here

			TargetUSDHr: target, ExistingUSDHr: existing, BaselineUSDHr: baseline}, trs, nil
	})
	require.NoError(f.t, err)
	return runID, ids
}

// amend sets the tranche to amount, moving its date one minute later.
func (f *ceilingFixture) amend(id string, amount float64) error {
	f.t.Helper()
	var rev int64
	var date time.Time
	require.NoError(f.t, f.store.db.QueryRow(f.ctx, "SELECT revision,scheduled_date FROM ladder_tranches WHERE id=$1", id).Scan(&rev, &date))
	_, err := f.store.AmendLadderTranche(f.ctx, id, f.accountID, "aws", "actor", LadderAmendment{
		ExpectedRevision: &rev, ScheduledDate: date.Add(time.Minute), AmountUSDHr: strconv.FormatFloat(amount, 'f', 6, 64)})
	return err
}

func (f *ceilingFixture) inFlight() float64 {
	f.t.Helper()
	v, err := inFlightLadderCommit(f.ctx, f.store.db, f.cfgID, time.Now())
	require.NoError(f.t, err)
	return v
}

func (f *ceilingFixture) move(id string, from, to ladder.TrancheStatus) {
	f.t.Helper()
	if from == ladder.TrancheStatusScheduled && to == ladder.TrancheStatusFired {
		// Claiming is the executor's job; the test only needs the resulting row state.
		_, err := f.store.db.Exec(f.ctx, "UPDATE ladder_tranches SET status='fired' WHERE id=$1", id)
		require.NoError(f.t, err)
		return
	}
	moved, err := f.store.TransitionLadderTranche(f.ctx, id, from, to, "test")
	require.NoError(f.t, err)
	require.True(f.t, moved)
}

func TestLadderAmendCeiling_ThreeStepReplay(t *testing.T) {
	f := newCeilingFixture(t)
	_, first := f.plan(fptr(10), fptr(0), fptr(100), 5, 5)
	require.NoError(t, f.amend(first[0], 1), "decrease")
	require.NoError(t, f.amend(first[0], 2), "raise within headroom (7 of 10)")
	require.NoError(t, f.amend(first[0], 1))
	// The planner tops up the gap: 6 in flight, 4 missing.
	f.plan(fptr(10), fptr(0), fptr(100), 4)
	require.Equal(t, 10.0, f.inFlight())
	require.ErrorIs(t, f.amend(first[0], 5), ErrLadderAmendInvalid, "raising A back would put 14 in flight against a 10 ceiling")
	require.Equal(t, 10.0, f.inFlight())
	require.NoError(t, f.amend(first[0], 1), "re-date only passes at the ceiling")
}

func TestLadderAmendCeiling_CompletedAfterRunCounts(t *testing.T) {
	f := newCeilingFixture(t)
	_, ids := f.plan(fptr(10), fptr(0), fptr(100), 5, 5)
	f.move(ids[0], ladder.TrancheStatusScheduled, ladder.TrancheStatusFired)
	require.ErrorIs(t, f.amend(ids[1], 6), ErrLadderAmendInvalid, "fired A (5) + B raised (6) > 10")
	f.move(ids[0], ladder.TrancheStatusFired, ladder.TrancheStatusCompleted)
	require.ErrorIs(t, f.amend(ids[1], 6), ErrLadderAmendInvalid, "A completed after the run started still counts: 5 + 6 > 10")
	require.NoError(t, f.amend(ids[1], 5), "re-date only")
	require.NoError(t, f.amend(ids[1], 4), "decrease")
	require.ErrorIs(t, f.amend(ids[0], 5), ErrLadderAmendConflict, "a completed tranche is not amendable")
}

func TestLadderAmendCeiling_SnapshotAndCoverage(t *testing.T) {
	t.Run("lowered target never blocks decrease or re-date", func(t *testing.T) {
		f := newCeilingFixture(t)
		_, ids := f.plan(fptr(10), fptr(0), fptr(100), 5, 5)
		_, err := f.store.db.Exec(f.ctx, "UPDATE ladder_runs SET target_usd_hr=4 WHERE config_id=$1", f.cfgID)
		require.NoError(t, err)
		require.NoError(t, f.amend(ids[0], 5), "re-date only with in-flight 10 > ceiling 4")
		require.ErrorIs(t, f.amend(ids[0], 6), ErrLadderAmendInvalid)
		require.NoError(t, f.amend(ids[0], 4))
	})
	t.Run("coverage lowered after the run caps a raise", func(t *testing.T) {
		f := newCeilingFixture(t)
		_, ids := f.plan(fptr(10), fptr(0), fptr(100), 1, 1)
		require.NoError(t, f.amend(ids[0], 3), "80% of 100 does not bind")
		_, err := f.store.db.Exec(f.ctx, "UPDATE ladder_configs SET target_coverage=4 WHERE id=$1", f.cfgID)
		require.NoError(t, err)
		require.ErrorIs(t, f.amend(ids[0], 4), ErrLadderAmendInvalid, "ceiling is now 4 but 5 would be in flight")
		require.NoError(t, f.amend(ids[0], 2))
	})
	t.Run("gross existing lowers the ceiling", func(t *testing.T) {
		f := newCeilingFixture(t)
		_, ids := f.plan(fptr(10), fptr(6), fptr(100), 1, 1)
		require.NoError(t, f.amend(ids[0], 3))
		require.ErrorIs(t, f.amend(ids[0], 4), ErrLadderAmendInvalid)
	})
	for name, snap := range map[string][3]*float64{
		"NULL target": {nil, fptr(0), fptr(100)}, "NULL existing": {fptr(10), nil, fptr(100)}, "NULL baseline": {fptr(10), fptr(0), nil},
	} {
		t.Run(name+" refuses a raise only", func(t *testing.T) {
			f := newCeilingFixture(t)
			_, ids := f.plan(snap[0], snap[1], snap[2], 2, 2)
			require.ErrorIs(t, f.amend(ids[0], 3), ErrLadderAmendInvalid)
			require.NoError(t, f.amend(ids[0], 2), "re-date only")
			require.NoError(t, f.amend(ids[0], 1), "decrease")
		})
	}
}

func TestLadderAmendCeiling_LatestRunDecides(t *testing.T) {
	f := newCeilingFixture(t)
	_, ids := f.plan(fptr(10), fptr(0), fptr(100), 1, 1)
	f.plan(fptr(3), fptr(0), fptr(100), 1)
	require.ErrorIs(t, f.amend(ids[0], 2), ErrLadderAmendInvalid, "latest run's ceiling 3 < 4 in flight, older run's 10 must not apply")
	runID, _ := f.plan(fptr(50), fptr(0), fptr(100))
	_, err := f.store.db.Exec(f.ctx, "UPDATE ladder_runs SET status='cancelled' WHERE id=$1", runID)
	require.NoError(t, err)
	require.ErrorIs(t, f.amend(ids[0], 2), ErrLadderAmendInvalid, "a canceled latest run is ignored")
}

func TestLadderAmendCeiling_WaitsForPlanner(t *testing.T) {
	f := newCeilingFixture(t)
	_, ids := f.plan(fptr(10), fptr(0), fptr(100), 5, 3)
	entered, gate, planned := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	asOf := time.Now().UTC().Truncate(time.Microsecond)
	go func() {
		runID := uuid.NewString()
		_, err := f.store.PlanAndSaveLadderRun(f.ctx, f.cfgID, asOf, func(inFlight float64) (*LadderRunDB, []LadderTrancheDB, error) {
			close(entered)
			<-gate
			tr := lifecycleTranche(f.cfgID, 10-inFlight, ladder.TrancheStatusScheduled, time.Now().UTC().Add(time.Hour))
			tr.RunID = &runID
			return &LadderRunDB{ID: runID, ConfigID: &f.cfgID, StartedAt: asOf, Status: ladder.RunStatusPlanned,
				TargetUSDHr: fptr(10), ExistingUSDHr: fptr(0), BaselineUSDHr: fptr(100)}, []LadderTrancheDB{tr}, nil
		})
		planned <- err
	}()
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release) // a failed wait below must not strand the planner
	<-entered
	amended := make(chan error, 1)
	go func() { amended <- f.amend(ids[0], 7) }()
	waitForLadderWaiters(f.ctx, t, f.store, f.cfgID, 1)
	release()
	require.NoError(t, <-planned)
	require.ErrorIs(t, <-amended, ErrLadderAmendInvalid, "amend must see the planner's committed top-up (8+2=10) and refuse +2")
	require.Equal(t, 10.0, f.inFlight())
}

func TestLadderAmendCeiling_LockTimeoutIsTyped(t *testing.T) {
	f := newCeilingFixture(t)
	_, ids := f.plan(fptr(10), fptr(0), fptr(100), 1)
	old := ladderLockTimeout
	ladderLockTimeout = 200 * time.Millisecond
	t.Cleanup(func() { ladderLockTimeout = old })
	holder := holdLadderLock(f.ctx, t, f.store, f.cfgID)
	var once sync.Once
	var holderErr error
	release := func() error { once.Do(func() { holderErr = holder() }); return holderErr }
	t.Cleanup(func() { _ = release() }) // a failed assertion must not leave the holder blocked
	err := f.amend(ids[0], 2)
	require.ErrorIs(t, err, ErrLadderLockTimeout, fmt.Sprint(err))
	require.NoError(t, release())
}

// settled_at is the database clock and the run's started_at the app clock. The
// offsets are literal on purpose: a margin of 0 misses the 30s case and a margin
// of an hour counts the 2m case.
func TestLadderAmendCeiling_SettledSkewMargin(t *testing.T) {
	for _, c := range []struct {
		name       string
		settledAgo time.Duration
		wantRaise  bool
	}{
		{"settled 30s before the run started still counts", 30 * time.Second, false},
		{"settled 2m before the run started does not count", 2 * time.Minute, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newCeilingFixture(t)
			runID, ids := f.plan(fptr(10), fptr(0), fptr(100), 5)
			var started time.Time
			require.NoError(t, f.store.db.QueryRow(f.ctx, "SELECT started_at FROM ladder_runs WHERE id=$1", runID).Scan(&started))
			done := lifecycleTranche(f.cfgID, 4, ladder.TrancheStatusCompleted, started.Add(-c.settledAgo))
			done.RunID = &runID
			require.NoError(t, f.store.SaveLadderTranches(f.ctx, []LadderTrancheDB{done}))
			err := f.amend(ids[0], 7)
			if c.wantRaise {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrLadderAmendInvalid)
			}
		})
	}
}

func TestLadderAmendCeiling_CancelsOverdueBeforeCounting(t *testing.T) {
	f := newCeilingFixture(t)
	_, ids := f.plan(fptr(10), fptr(0), fptr(100), 5, 5)
	// X is past the 24h grace window: neither counted in flight nor claimable at "now".
	_, err := f.store.db.Exec(f.ctx, "UPDATE ladder_tranches SET scheduled_date=now()-interval '25 hours' WHERE id=$1", ids[0])
	require.NoError(t, err)
	require.NoError(t, f.amend(ids[1], 10), "X is not in flight, so 10 fits the ceiling")
	var status ladder.TrancheStatus
	require.NoError(t, f.store.db.QueryRow(f.ctx, "SELECT status FROM ladder_tranches WHERE id=$1", ids[0]).Scan(&status))
	require.Equal(t, ladder.TrancheStatusCancelled, status, "the overdue row must be canceled, not left for a claim with an older asOf")
	claimed, err := f.store.ClaimDueLadderTranches(f.ctx, f.cfgID, time.Now().Add(-time.Minute), 10,
		func(context.Context, pgx.Tx, LadderTrancheDB) (string, error) { return uuid.NewString(), nil })
	require.NoError(t, err)
	require.Empty(t, claimed)
}

func TestLadderAmendCeiling_CeilingTruncatesNotRounds(t *testing.T) {
	f := newCeilingFixture(t)
	// 1.000015 * 33.33% = 0.3333049995: truncated 0.333304, rounded 0.333305.
	_, err := f.store.db.Exec(f.ctx, "UPDATE ladder_configs SET target_coverage=33.33 WHERE id=$1", f.cfgID)
	require.NoError(t, err)
	_, ids := f.plan(fptr(10), fptr(0), fptr(1.000015), 0.1)
	require.ErrorIs(t, f.amend(ids[0], 0.333305), ErrLadderAmendInvalid)
	require.NoError(t, f.amend(ids[0], 0.333304))
}

func TestLadderAmendCeiling_HugeAmountRefused(t *testing.T) {
	f := newCeilingFixture(t)
	_, ids := f.plan(fptr(1e13), fptr(0), fptr(9e13), 1)
	_, err := f.store.db.Exec(f.ctx, "UPDATE ladder_runs SET total_hourly_commit=99999999999999 WHERE config_id=$1", f.cfgID)
	require.NoError(t, err) // let the original-total term pass so only the guard can refuse
	require.ErrorIs(t, f.amend(ids[0], 9999999999999), ErrLadderAmendInvalid)
}
