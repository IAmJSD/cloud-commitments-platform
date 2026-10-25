//go:build integration

package purchase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

const failAtomicReason = "plan step can never be bought"

// releaseHookStore runs hook at the start of the suppression release that
// failExistingPlanStep does after its failed CAS. A returned error stands in
// for a crash or DB error at that point; a blocking hook holds the writer
// there so a test can look at what other connections see.
type releaseHookStore struct {
	*config.PostgresStore
	hook func() error
}

func (s *releaseHookStore) DeleteSuppressionsByExecutionTx(ctx context.Context, tx pgx.Tx, executionID string) error {
	if err := s.hook(); err != nil {
		return err
	}
	return s.PostgresStore.DeleteSuppressionsByExecutionTx(ctx, tx, executionID)
}

// failRowState is what a plan-step root looks like from outside the writer.
type failRowState struct {
	Status       string
	Error        string
	NoSubmission bool
	Suppressions int
}

func (f *cumFixture) failRowState(t *testing.T, executionID string) failRowState {
	t.Helper()
	var st failRowState
	require.NoError(t, f.pool.QueryRow(f.ctx, `
		SELECT status, COALESCE(error, ''), COALESCE(no_submission, false),
		       (SELECT COUNT(*) FROM purchase_suppressions WHERE execution_id = $1)
		  FROM purchase_executions WHERE execution_id = $1`, executionID).
		Scan(&st.Status, &st.Error, &st.NoSubmission, &st.Suppressions))
	return st
}

// seedAttachedRoot saves a pending root row carrying recommendations and one
// suppression, as a refused attached step looks before it is failed.
func (f *cumFixture) seedAttachedRoot(t *testing.T) *config.PurchaseExecution {
	t.Helper()
	rec := cumRec(f.account, "m5.t0", 100)
	rec.Selected = true
	root := &config.PurchaseExecution{PlanID: f.planID, ExecutionID: uuid.NewString(), Status: "pending", StepNumber: 1,
		ScheduledDate: time.Now().Add(40 * 24 * time.Hour), Recommendations: []config.RecommendationRecord{rec}}
	require.NoError(t, f.store.SavePurchaseExecution(f.ctx, root))
	require.NoError(t, f.store.CreateSuppression(f.ctx, &config.PurchaseSuppression{ExecutionID: root.ExecutionID, AccountID: "sup-acct",
		Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.t0", SuppressedCount: 1, ExpiresAt: time.Now().Add(time.Hour)}))
	return root
}

var stampedFailure = failRowState{Status: "failed", Error: failAtomicReason, NoSubmission: true, Suppressions: 0}

// Issue #1052: an error between the failed CAS and the stamp/release used to
// leave the row failed with no reason, no NoSubmission and its suppressions in
// place, and no later tick adopts a failed root. Now nothing commits, so the
// row is still pending and the next attempt fails it properly.
func TestFailExistingPlanStep_ReleaseErrorCommitsNothing(t *testing.T) {
	f := newCumFixture(t, 100)
	root := f.seedAttachedRoot(t)
	injected := errors.New("connection lost before the suppression release")
	f.m.config = &releaseHookStore{PostgresStore: f.store, hook: func() error { return injected }}

	err := f.m.failExistingPlanStep(f.ctx, root, failAtomicReason)
	require.ErrorIs(t, err, injected)
	require.NotErrorIs(t, err, errExecutionNotNotifiable, "a failed write is not a quiet skip")
	require.Equal(t, failRowState{Status: "pending", Suppressions: 1}, f.failRowState(t, root.ExecutionID),
		"the CAS rolls back with the stamp and the release")

	f.m.config = f.store
	err = f.m.failExistingPlanStep(f.ctx, root, failAtomicReason)
	require.ErrorIs(t, err, errExecutionNotNotifiable)
	require.Contains(t, err.Error(), failAtomicReason)
	require.Equal(t, stampedFailure, f.failRowState(t, root.ExecutionID), "the retry fails the still-pending row")
}

// While the fail is between its CAS and its commit, other connections still
// see the pending row, and an approval waits on the row lock and then loses to
// the committed failure instead of landing on a half-written one.
func TestFailExistingPlanStep_ConcurrentApproveWaitsForStampedFailure(t *testing.T) {
	f := newCumFixture(t, 100)
	root := f.seedAttachedRoot(t)
	reached, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock) // a failed assertion must not leave the writer parked
	f.m.config = &releaseHookStore{PostgresStore: f.store, hook: func() error {
		close(reached)
		<-release
		return nil
	}}
	failErr := make(chan error, 1)
	go func() { failErr <- f.m.failExistingPlanStep(f.ctx, root, failAtomicReason) }()
	<-reached

	require.Equal(t, failRowState{Status: "pending", Suppressions: 1}, f.failRowState(t, root.ExecutionID),
		"no other connection sees a failed row before its reason and release commit")
	approveErr := make(chan error, 1)
	go func() {
		_, err := f.store.TransitionExecutionStatus(f.ctx, root.ExecutionID, []string{"pending", "notified"}, "approved", nil)
		approveErr <- err
	}()
	f.requireApproveBlocked(t, approveErr)
	unblock()

	err := <-failErr
	require.ErrorIs(t, err, errExecutionNotNotifiable)
	require.Contains(t, err.Error(), failAtomicReason)
	require.ErrorIs(t, <-approveErr, config.ErrExecutionNotInExpectedStatus)
	require.Equal(t, stampedFailure, f.failRowState(t, root.ExecutionID))
}

// requireApproveBlocked waits until the approval's UPDATE is waiting on a row
// lock. An approval that returns first did not wait for the fail to commit.
func (f *cumFixture) requireApproveBlocked(t *testing.T, approveErr chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-approveErr:
			t.Fatalf("approval returned (%v) while the fail was still uncommitted", err)
		default:
		}
		var waiting int
		require.NoError(t, f.pool.QueryRow(f.ctx, `
			SELECT COUNT(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'
			   AND query LIKE '%UPDATE purchase_executions%'`).Scan(&waiting))
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("approval never waited on the row lock")
}

// failRacer is a writer that competes with failExistingPlanStep for a pending
// root. It reports whether it won and the state its win leaves behind.
type failRacer struct {
	name string
	run  func(f *cumFixture, root *config.PurchaseExecution) (won bool, err error)
	want failRowState
}

var failRacers = []failRacer{
	{name: "approve", want: failRowState{Status: "approved", Suppressions: 1},
		run: func(f *cumFixture, root *config.PurchaseExecution) (bool, error) {
			_, err := f.store.TransitionExecutionStatus(f.ctx, root.ExecutionID, []string{"pending", "notified"}, "approved", nil)
			if errors.Is(err, config.ErrExecutionNotInExpectedStatus) {
				return false, nil
			}
			return err == nil, err
		}},
	{name: "cancel", want: failRowState{Status: config.StatusCanceled, Suppressions: 0},
		run: func(f *cumFixture, root *config.PurchaseExecution) (bool, error) {
			var won bool
			err := f.store.WithTx(f.ctx, func(tx pgx.Tx) error {
				var cancelErr error
				if won, _, cancelErr = f.store.CancelExecutionAtomic(f.ctx, tx, root.ExecutionID, nil); cancelErr != nil || !won {
					return cancelErr
				}
				return f.store.DeleteSuppressionsByExecutionTx(f.ctx, tx, root.ExecutionID)
			})
			return won, err
		}},
	{name: "expiry sweep", want: failRowState{Status: config.StatusExpired, Suppressions: 0},
		run: func(f *cumFixture, root *config.PurchaseExecution) (bool, error) {
			// The same transaction as the history handler's expireStaleExecutionsSweep.
			var won bool
			err := f.store.WithTx(f.ctx, func(tx pgx.Tx) error {
				var expErr error
				if won, expErr = f.store.ExpireExecutionAtomic(f.ctx, tx, root.ExecutionID); expErr != nil || !won {
					return expErr
				}
				return f.store.DeleteSuppressionsByExecutionTx(f.ctx, tx, root.ExecutionID)
			})
			return won, err
		}},
	{name: "second tick", want: stampedFailure,
		run: func(f *cumFixture, root *config.PurchaseExecution) (bool, error) {
			won, err := failStepOutcome(f.m.failExistingPlanStep(f.ctx, root, failAtomicReason))
			return won, err
		}},
}

// failStepOutcome classifies a failExistingPlanStep result: a won fail is the
// bare step failure, a lost CAS is the changed-state skip.
func failStepOutcome(err error) (won bool, unexpected error) {
	switch {
	case err != nil && errors.Is(err, errExecutionNotNotifiable) && strings.Contains(err.Error(), "changed state before it could be failed"):
		return false, nil
	case err != nil && errors.Is(err, errExecutionNotNotifiable) && strings.Contains(err.Error(), failAtomicReason):
		return true, nil
	default:
		return false, fmt.Errorf("unexpected fail outcome: %w", err)
	}
}

// Real-Postgres race of the fail against each writer that can take a pending
// root (approval, cancel, the stale-execution expiry sweep, another tick): exactly one wins every round, the row ends in that winner's complete
// state, and no round deadlocks or leaves a failed row unstamped.
func TestFailExistingPlanStep_RacesEndInOneCompleteState(t *testing.T) {
	f := newCumFixture(t, 100)
	const rounds = 25
	for _, racer := range failRacers {
		t.Run(racer.name, func(t *testing.T) {
			failWins := 0
			defer func() { t.Logf("fail won %d of %d rounds against %s", failWins, rounds, racer.name) }()
			for i := 0; i < rounds; i++ {
				root := f.seedAttachedRoot(t)
				var wg sync.WaitGroup
				start := make(chan struct{})
				var failWon, racerWon bool
				var failErr, racerErr error
				wg.Add(2)
				go func() {
					defer wg.Done()
					<-start
					failWon, failErr = failStepOutcome(f.m.failExistingPlanStep(f.ctx, root, failAtomicReason))
				}()
				// Staggering the racer by 0-2ms lets both sides win some rounds:
				// the fail opens its transaction first, so without it the
				// single-statement approval always lands first.
				stagger := time.Duration(i%3) * time.Millisecond
				go func() {
					defer wg.Done()
					<-start
					time.Sleep(stagger)
					racerWon, racerErr = racer.run(f, root)
				}()
				close(start)
				wg.Wait()
				require.NoError(t, failErr, "round %d", i)
				require.NoError(t, racerErr, "round %d", i)
				require.True(t, failWon != racerWon, "round %d: exactly one writer wins (fail=%v %s=%v)", i, failWon, racer.name, racerWon)
				want := racer.want
				if failWon {
					failWins++
					want = stampedFailure
				}
				require.Equal(t, want, f.failRowState(t, root.ExecutionID), "round %d (fail won: %v)", i, failWon)
			}
		})
	}
}
