//go:build integration

package purchase

// Real PostgreSQL plus a counting fake provider (labeled fixture; nothing here
// reaches a cloud API). Issue #829: every executor claims the row "running"
// before any provider call, so one row is bought at most once.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func claimRaceRow(ctx context.Context, t *testing.T, f *rampStepFixture, status string) *config.PurchaseExecution {
	t.Helper()
	f.repairAccounts(ctx, t)
	row := &config.PurchaseExecution{
		PlanID: f.plan.ID, ExecutionID: uuid.New().String(), Status: status, StepNumber: 3,
		ScheduledDate: time.Now(), CloudAccountID: &f.accounts[0].ID,
		Recommendations: []config.RecommendationRecord{withAccount(rampStepRecommendation()[0], f.accounts[0].ID)},
	}
	require.NoError(t, f.store.SavePurchaseExecution(ctx, row))
	return row
}

func rowStatus(ctx context.Context, t *testing.T, f *rampStepFixture, id string) *config.PurchaseExecution {
	t.Helper()
	row, err := f.store.GetExecutionByID(ctx, id)
	require.NoError(t, err)
	return row
}

// Both orderings of the approve/claimAndExecute race: the second executor loses
// the claim and never reaches the provider. Before the fix the approve path ran
// an "approved" row unclaimed, so the second ordering bought it twice.
func TestSingleClaim_BothOrderingsBuyOnce(t *testing.T) {
	ctx := context.Background()
	f := newRampStepFixture(ctx, t)
	global, err := f.store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	require.True(t, global.AutoPurchaseEnabled)

	t.Run("claimAndExecute then approve", func(t *testing.T) {
		row := claimRaceRow(ctx, t, f, "notified")
		before := f.purchaseCount()
		claimed, execErr := f.manager.claimAndExecute(ctx, rowStatusCopy(row))
		require.NoError(t, execErr)
		require.True(t, claimed)
		_, _, err := f.manager.ApproveAndExecute(ctx, row.ExecutionID, "approver@example.invalid", nil, nil)
		require.ErrorIs(t, err, config.ErrExecutionNotInExpectedStatus)
		assert.Equal(t, 1, f.purchaseCount()-before)
		assert.Equal(t, "completed", rowStatus(ctx, t, f, row.ExecutionID).Status)
	})

	t.Run("approve then claimAndExecute", func(t *testing.T) {
		row := claimRaceRow(ctx, t, f, "notified")
		before := f.purchaseCount()
		_, _, err := f.manager.ApproveAndExecute(ctx, row.ExecutionID, "approver@example.invalid", nil, nil)
		require.NoError(t, err)
		claimed, execErr := f.manager.claimAndExecute(ctx, rowStatusCopy(row))
		require.NoError(t, execErr)
		assert.False(t, claimed, "the approved-and-run row must not be claimable again")
		assert.Equal(t, 1, f.purchaseCount()-before)
		got := rowStatus(ctx, t, f, row.ExecutionID)
		assert.Equal(t, "completed", got.Status)
		require.NotNil(t, got.ApprovedBy, "the audit stamp must survive and not revert the status")
	})
}

func rowStatusCopy(r *config.PurchaseExecution) *config.PurchaseExecution { c := *r; return &c }

// N goroutines, every executor that may claim the starting status: exactly one
// provider call and one terminal write.
func TestSingleClaim_ConcurrentExecutorsBuyOnce(t *testing.T) {
	ctx := context.Background()
	f := newRampStepFixture(ctx, t)
	const n = 4
	cases := map[string][]func(row *config.PurchaseExecution){}
	approve := func(row *config.PurchaseExecution) {
		_, _, _ = f.manager.ApproveAndExecute(ctx, row.ExecutionID, "a@example.invalid", nil, nil)
	}
	direct := func(row *config.PurchaseExecution) {
		_, _, _ = f.manager.DirectExecute(ctx, row.ExecutionID, "a@example.invalid", nil, nil)
	}
	cron := func(row *config.PurchaseExecution) { _, _ = f.manager.claimAndExecute(ctx, rowStatusCopy(row)) }
	redrive := func(row *config.PurchaseExecution) { _, _ = f.manager.claimAndRedrive(ctx, rowStatusCopy(row)) }
	fire := func(row *config.PurchaseExecution) { f.manager.fireOneDue(ctx, rowStatusCopy(row)) }
	cases["notified"] = []func(*config.PurchaseExecution){approve, direct, cron}
	cases["approved"] = []func(*config.PurchaseExecution){cron, redrive}
	cases["scheduled"] = []func(*config.PurchaseExecution){fire}

	for status, executors := range cases {
		t.Run(status, func(t *testing.T) {
			row := claimRaceRow(ctx, t, f, status)
			before := f.purchaseCount()
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				for _, run := range executors {
					wg.Add(1)
					go func() { defer wg.Done(); run(row) }()
				}
			}
			wg.Wait()
			assert.Equal(t, 1, f.purchaseCount()-before, "exactly one provider call for one row")
			assert.Equal(t, "completed", rowStatus(ctx, t, f, row.ExecutionID).Status)
		})
	}
}

// reapingStore lets the test hold the reaper's annotation until the executor
// has finished, producing the order: reaper CAS -> executor recovery -> reaper
// annotation.
type reapingStore struct {
	*config.PostgresStore
	release chan struct{}
}

func (r *reapingStore) SaveExecutionIfStatus(ctx context.Context, e *config.PurchaseExecution, expected []string) error {
	if len(expected) == 1 && expected[0] == "failed" {
		<-r.release
	}
	return r.PostgresStore.SaveExecutionIfStatus(ctx, e, expected)
}

// The reaper fails the row mid-run, the provider call still succeeds: the
// executor recovers the row to completed with its own note, advances the ramp,
// and the reaper's late annotation neither overwrites it nor counts as Errored.
// The fan-out case is the realistic one: the root row carries no purchased recs
// of its own, only its per-account copies do.
func TestSingleClaim_ReaperInterleavingRecoversPurchasedRow(t *testing.T) {
	for _, tc := range []struct {
		name      string
		purchases int
		newRow    func(ctx context.Context, t *testing.T, f *rampStepFixture) *config.PurchaseExecution
	}{
		{"single account", 1, func(ctx context.Context, t *testing.T, f *rampStepFixture) *config.PurchaseExecution {
			return claimRaceRow(ctx, t, f, "notified")
		}},
		{"multi-account root", 3, func(ctx context.Context, t *testing.T, f *rampStepFixture) *config.PurchaseExecution {
			f.repairAccounts(ctx, t)
			return f.saveRootExecution(ctx, t, 3)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newRampStepFixture(ctx, t)
			row := tc.newRow(ctx, t, f)
			stepBefore := f.currentStep(ctx, t)
			rs := &reapingStore{PostgresStore: f.store, release: make(chan struct{})}
			f.manager.config = rs

			result := &ReapResult{}
			reaped := make(chan struct{})
			var fired atomic.Bool
			f.onPurchase = func() { // fires inside the first provider call
				if fired.Swap(true) {
					return
				}
				stuck := rowStatusCopy(row)
				stuck.Status = "running"
				go func() { defer close(reaped); f.manager.reapOne(ctx, stuck, 10*time.Minute, time.Now(), result) }()
				require.Eventually(t, func() bool { return rowStatus(ctx, t, f, row.ExecutionID).Status == "failed" }, 5*time.Second, 10*time.Millisecond)
			}

			_, _, err := f.manager.ApproveAndExecute(ctx, row.ExecutionID, "a@example.invalid", nil, nil)
			require.NoError(t, err)
			close(rs.release)
			<-reaped

			got := rowStatus(ctx, t, f, row.ExecutionID)
			assert.Equal(t, "completed", got.Status)
			assert.Contains(t, got.Error, recoveredAfterReapNote)
			assert.Equal(t, tc.purchases, f.purchaseCount())
			assert.Equal(t, 1, result.Reaped)
			assert.Zero(t, result.Errored)
			assert.Equal(t, stepBefore+1, f.currentStep(ctx, t), "recovery must still advance the ramp")
		})
	}
}

// Deterministic replay of the #829 race: while the approve path is inside its
// provider call, another executor (cron/SQS) tries to claim the same row. On the
// old code the row sat "approved" and the second executor bought it again.
func TestSingleClaim_ExecutorArrivingMidApproveCannotBuyAgain(t *testing.T) {
	ctx := context.Background()
	f := newRampStepFixture(ctx, t)
	row := claimRaceRow(ctx, t, f, "notified")
	var fired atomic.Bool
	var midClaimed bool
	f.onPurchase = func() {
		if fired.Swap(true) {
			return
		}
		c := rowStatusCopy(row)
		c.Status = "approved"
		midClaimed, _ = f.manager.claimAndExecute(ctx, c)
	}
	_, _, err := f.manager.ApproveAndExecute(ctx, row.ExecutionID, "a@example.invalid", nil, nil)
	require.NoError(t, err)
	assert.False(t, midClaimed, "the row is running, so a second executor loses the claim")
	assert.Equal(t, 1, f.purchaseCount())
	assert.Equal(t, "completed", rowStatus(ctx, t, f, row.ExecutionID).Status)
}

// Issue #532 item 4: the stranded-approval re-drive claims only "approved"
// rows, and every synchronous run (approve, direct execute, run-now) holds its
// row "running" for the whole provider call, so a re-drive that read the row
// as approved (a stale listing) loses the claim mid-run and buys nothing.
func TestSingleClaim_StrandedRedriveMidSyncRunCannotBuyAgain(t *testing.T) {
	ctx := context.Background()
	f := newRampStepFixture(ctx, t)
	runs := map[string]func(id string) error{
		"approve": func(id string) error {
			_, _, err := f.manager.ApproveAndExecute(ctx, id, "a@example.invalid", nil, nil)
			return err
		},
		"direct execute": func(id string) error {
			_, _, err := f.manager.DirectExecute(ctx, id, "a@example.invalid", nil, nil)
			return err
		},
		"run now": func(id string) error {
			_, _, err := f.manager.RunPlannedPurchaseNow(ctx, id, "a@example.invalid", nil, nil)
			return err
		},
	}
	for name, run := range runs {
		t.Run(name, func(t *testing.T) {
			row := claimRaceRow(ctx, t, f, "notified")
			before := f.purchaseCount()
			var fired atomic.Bool
			var midStatus string
			var redriven bool
			var redriveErr error
			f.onPurchase = func() {
				if fired.Swap(true) {
					return
				}
				midStatus = rowStatus(ctx, t, f, row.ExecutionID).Status
				c := rowStatusCopy(row)
				c.Status = "approved"
				redriven, redriveErr = f.manager.claimAndRedrive(ctx, c)
			}
			t.Cleanup(func() { f.onPurchase = nil })
			require.NoError(t, run(row.ExecutionID))
			assert.Equal(t, "running", midStatus, "the sync run holds the row running during the provider call")
			require.NoError(t, redriveErr)
			assert.False(t, redriven, "the re-drive must lose the claim")
			assert.Equal(t, 1, f.purchaseCount()-before)
			assert.Equal(t, "completed", rowStatus(ctx, t, f, row.ExecutionID).Status)
		})
	}
}
