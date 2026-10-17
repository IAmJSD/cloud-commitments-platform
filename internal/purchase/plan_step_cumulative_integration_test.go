//go:build integration

package purchase

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Real PostgreSQL and the real planstep resolver. Only the recommendation
// reads (the provider payload `shown` and the stored rows) are fixtures; the
// provider purchase itself is simulated by marking the attached row completed.

type cumSource struct {
	mu    sync.Mutex
	shown func() []config.RecommendationRecord
}

func (s *cumSource) ListRecommendationsStrict(context.Context, config.RecommendationFilter) ([]config.RecommendationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shown(), nil
}

type cumStore struct {
	rows []config.RecommendationRecord
}

func (s cumStore) ListStoredRecommendations(context.Context, config.RecommendationFilter) ([]config.RecommendationRecord, error) {
	return s.rows, nil
}
func (s cumStore) GetGlobalConfig(context.Context) (*config.GlobalConfig, error) {
	return &config.GlobalConfig{}, nil
}

type cumFixture struct {
	t       *testing.T
	ctx     context.Context
	store   *config.PostgresStore
	pool    *pgxpool.Pool
	m       *Manager
	planID  string
	account string
	src     *cumSource
}

// cumRec is a 100-unit-style rec of one instance type; the stored row holds the
// full demand and prices every share.
func cumRec(account, typ string, count int) config.RecommendationRecord {
	return config.RecommendationRecord{ID: "rec-" + typ, Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: typ,
		Count: count, Term: 1, Payment: "no-upfront", UpfrontCost: float64(count), Savings: float64(count) / 10, CloudAccountID: &account}
}

// cumCoverage is the plan coverage every scenario sizes against.
const cumCoverage = 80

func newCumFixture(t *testing.T, stored ...int) *cumFixture {
	t.Helper()
	store, pool := rampStepStore(context.Background(), t)
	return newCumFixtureOn(t, store, pool, stored...)
}

// newCumFixtureOn builds the fixture on an already migrated store.
func newCumFixtureOn(t *testing.T, store *config.PostgresStore, pool *pgxpool.Pool, stored ...int) *cumFixture {
	t.Helper()
	ctx := context.Background()
	attachGlobal(ctx, t, store)
	acct := &config.CloudAccount{Name: "cum-acct", Provider: "aws", ExternalID: "111111111110", Enabled: true,
		AWSAuthMode: "role_arn", AWSRoleARN: "arn:aws:iam::111111111110:role/cudly"}
	require.NoError(t, store.CreateCloudAccount(ctx, acct))

	next := time.Now().Add(24 * time.Hour).Truncate(time.Microsecond)
	plan := &config.PurchasePlan{
		Name: "cumulative", Enabled: true, AutoPurchase: true, NotificationDaysBefore: 30, MaxStepCommitment: f64(1e9),
		Services: map[string]config.ServiceConfig{"ec2": {Provider: "aws", Service: "ec2", Enabled: true, Term: 1, Payment: "no-upfront", Coverage: cumCoverage}},
		RampSchedule: config.RampSchedule{Type: "weekly", PercentPerStep: 25, StepIntervalDays: 7, TotalSteps: 4,
			StartDate: next.AddDate(0, 0, -7)},
		NextExecutionDate: &next,
	}
	require.NoError(t, plan.Validate())
	require.NoError(t, store.CreatePurchasePlan(ctx, plan))
	require.NoError(t, store.SetPlanAccounts(ctx, plan.ID, []string{acct.ID}))

	f := &cumFixture{t: t, ctx: ctx, store: store, pool: pool, planID: plan.ID, account: acct.ID, src: &cumSource{}}
	rows := make([]config.RecommendationRecord, 0, len(stored))
	for i, n := range stored {
		rows = append(rows, cumRec(acct.ID, fmt.Sprintf("m5.t%d", i), n))
	}
	f.m = &Manager{config: store, email: newCountingSender(ctx)}
	f.m.resolver = planstepResolver{src: f.src, store: cumStore{rows: rows}}
	return f
}

func (f *cumFixture) plan() *config.PurchasePlan {
	f.t.Helper()
	p, err := f.store.GetPurchasePlan(f.ctx, f.planID)
	require.NoError(f.t, err)
	return p
}

func (f *cumFixture) stepRows(step int) []config.PurchaseExecution {
	f.t.Helper()
	var rows []config.PurchaseExecution
	require.NoError(f.t, f.store.WithTx(f.ctx, func(tx pgx.Tx) error {
		var err error
		rows, err = f.store.ListExecutionsForPlanStepTx(f.ctx, tx, f.planID, step)
		return err
	}))
	return rows
}

// buy simulates the provider purchase of the step's attached row and counts
// the step, exactly as the executor does on success.
func (f *cumFixture) buy(step int) {
	f.t.Helper()
	_, err := f.pool.Exec(f.ctx,
		`UPDATE purchase_executions SET status = 'completed' WHERE plan_id = $1 AND step_number = $2`, f.planID, step)
	require.NoError(f.t, err)
	require.NoError(f.t, f.store.CompletePlanStep(f.ctx, f.planID, step))
}

// tick runs one notification tick and returns the attached row, or the error.
func (f *cumFixture) tick() (*config.PurchaseExecution, error) {
	f.t.Helper()
	exec, _, _, err := f.m.getOrCreateExecution(f.ctx, f.plan())
	return exec, err
}

func totalCount(e *config.PurchaseExecution) (n int) {
	for i := range e.Recommendations {
		n += e.Recommendations[i].Count
	}
	return n
}

// shownAfterSuppression models the real read: demand minus what earlier steps
// attached (their suppressions are still live).
func (f *cumFixture) shownSuppressed(demand int) {
	f.src.shown = func() []config.RecommendationRecord {
		bought := 0
		for step := 1; step <= 4; step++ {
			for _, r := range f.stepRows(step) {
				bought += totalCount(&r)
			}
		}
		return []config.RecommendationRecord{cumRec(f.account, "m5.t0", demand-bought)}
	}
}

func TestCumulativeAttach_WeeklyRampBuysTwentyEachStep(t *testing.T) {
	for name, setup := range map[string]func(f *cumFixture){
		"suppression live (shown shrinks)": func(f *cumFixture) { f.shownSuppressed(100) },
		"suppression expired (shown lagging)": func(f *cumFixture) {
			f.src.shown = func() []config.RecommendationRecord {
				return []config.RecommendationRecord{cumRec(f.account, "m5.t0", 100)}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCumFixture(t, 100)
			setup(f)
			for step := 1; step <= 4; step++ {
				exec, err := f.tick()
				require.NoError(t, err, "step %d", step)
				assert.Equal(t, step, exec.StepNumber)
				assert.Equal(t, 20, totalCount(exec), "step %d buys the gap to its cumulative target, not a share of the remainder", step)
				f.buy(step)
			}
			assert.Equal(t, 4, f.plan().RampSchedule.CurrentStep)
			var raw []byte
			require.NoError(t, f.pool.QueryRow(f.ctx, `SELECT ramp_baseline FROM purchase_plans WHERE id=$1`, f.planID).Scan(&raw))
			var base config.PlanRampBaseline
			require.NoError(t, json.Unmarshal(raw, &base))
			require.Len(t, base, 1)
			for _, q := range base {
				assert.Equal(t, 100, q.Count, "frozen at step 1 from shown")
			}
		})
	}
}

func (f *cumFixture) baseline() (b config.PlanRampBaseline, err error) {
	err = f.store.WithTx(f.ctx, func(tx pgx.Tx) error {
		b, err = f.store.GetPlanRampBaselineTx(f.ctx, tx, f.planID)
		return err
	})
	return b, err
}

func TestCumulativeAttach_TargetMetCompletesNoOpAndAdvances(t *testing.T) {
	f := newCumFixture(t, 100)
	f.src.shown = func() []config.RecommendationRecord {
		return []config.RecommendationRecord{cumRec(f.account, "m5.t0", 100)}
	}
	exec, err := f.tick()
	require.NoError(t, err)
	require.Equal(t, 20, totalCount(exec))
	f.buy(1)

	// Another row of this plan (step 5, outside the ramp) already holds 20 more.
	extra := &config.PurchaseExecution{PlanID: f.planID, ExecutionID: "00000000-0000-4000-8000-00000000e001", Status: "completed", StepNumber: 5,
		ScheduledDate: time.Now(), Recommendations: []config.RecommendationRecord{cumRec(f.account, "m5.t0", 20)}}
	extra.Recommendations[0].Selected = true
	require.NoError(t, f.store.SavePurchaseExecution(f.ctx, extra))
	// An unknown-outcome failed row (no_submission false) is counted as held.
	unknown := &config.PurchaseExecution{PlanID: f.planID, ExecutionID: "00000000-0000-4000-8000-00000000e002", Status: "failed", StepNumber: 6,
		ScheduledDate: time.Now(), Recommendations: []config.RecommendationRecord{cumRec(f.account, "m5.t0", 0)}}
	require.NoError(t, f.store.SavePurchaseExecution(f.ctx, unknown))
	// A failed row proven unsubmitted holds nothing and is not counted in the note.
	unsubmitted := &config.PurchaseExecution{PlanID: f.planID, ExecutionID: "00000000-0000-4000-8000-00000000e003", Status: "failed", StepNumber: 7,
		ScheduledDate: time.Now(), NoSubmission: true, Recommendations: []config.RecommendationRecord{cumRec(f.account, "m5.t0", 9)}}
	require.NoError(t, f.store.SavePurchaseExecution(f.ctx, unsubmitted))

	_, err = f.tick()
	require.ErrorIs(t, err, errExecutionNotNotifiable, "nothing to notify or buy")

	rows := f.stepRows(2)
	require.Len(t, rows, 1)
	assert.Equal(t, "completed", rows[0].Status)
	assert.Empty(t, rows[0].Recommendations)
	require.NotNil(t, rows[0].PreApprovalSkipReason)
	assert.Equal(t, config.PreApprovalSkipRampTargetMet, *rows[0].PreApprovalSkipReason)
	assert.Contains(t, rows[0].Error, "target 40, committed 40")
	assert.Contains(t, rows[0].Error, "1 failed row(s) of unknown outcome counted as committed")
	assert.Equal(t, 2, f.plan().RampSchedule.CurrentStep, "the ramp advanced over the no-op")

	exec, err = f.tick()
	require.NoError(t, err)
	assert.Equal(t, 3, exec.StepNumber)
	assert.Equal(t, 20, totalCount(exec), "step 3 target 60 minus 40 held")
}

func TestCumulativeAttach_StepOneNoMatchWithBaselineFreezesAndStepTwoBuys(t *testing.T) {
	f := newCumFixture(t, 3)
	f.src.shown = func() []config.RecommendationRecord {
		return []config.RecommendationRecord{cumRec(f.account, "m5.t0", 3)}
	}

	// floor(3*80*25/10000) = 0: step 1 has nothing to buy but is a legitimate no-op.
	_, err := f.tick()
	require.ErrorIs(t, err, errExecutionNotNotifiable)
	base, err := f.baseline()
	require.NoError(t, err)
	require.Len(t, base, 1, "the baseline is frozen although step 1 sized to nothing")
	rows := f.stepRows(1)
	require.Len(t, rows, 1)
	assert.Equal(t, "completed", rows[0].Status)
	assert.Equal(t, 1, f.plan().RampSchedule.CurrentStep)

	exec, err := f.tick()
	require.NoError(t, err, "step 2 must not strand for want of a baseline")
	assert.Equal(t, 1, totalCount(exec), "floor(3*80*50/10000) = 1")
}

func TestCumulativeAttach_HiddenIdentityIsNoMatchNotNoOp(t *testing.T) {
	f := newCumFixture(t, 100)
	visible := true
	f.src.shown = func() []config.RecommendationRecord {
		if !visible {
			return nil
		}
		return []config.RecommendationRecord{cumRec(f.account, "m5.t0", 100)}
	}
	_, err := f.tick()
	require.NoError(t, err)
	f.buy(1)

	visible = false // suppressed elsewhere or a lagging payload: gap 20, shown 0
	_, err = f.tick()
	require.ErrorIs(t, err, errPlanStepSkipped, "inside the grace window: retry, never a no-op advance")
	assert.Empty(t, f.stepRows(2))
	assert.Equal(t, 1, f.plan().RampSchedule.CurrentStep)

	_, perr := f.pool.Exec(f.ctx, `UPDATE purchase_plans SET next_execution_date = now() - interval '30 days' WHERE id = $1`, f.planID)
	require.NoError(t, perr)
	_, err = f.tick()
	require.ErrorIs(t, err, errExecutionNotNotifiable)
	rows := f.stepRows(2)
	require.Len(t, rows, 1)
	assert.Equal(t, "failed", rows[0].Status, "after the grace window one visible failed row")
	assert.Equal(t, 1, f.plan().RampSchedule.CurrentStep, "a failed step does not advance the ramp")
}

func TestCumulativeAttach_NewIdentityAfterStepOneIsSkippedAndNoted(t *testing.T) {
	f := newCumFixture(t, 100, 50)
	only := true
	f.src.shown = func() []config.RecommendationRecord {
		if only {
			return []config.RecommendationRecord{cumRec(f.account, "m5.t0", 100)}
		}
		return []config.RecommendationRecord{cumRec(f.account, "m5.t1", 50)}
	}
	_, err := f.tick()
	require.NoError(t, err)
	f.buy(1)

	only = false // step 2 sees only an identity first shown now; the baselined one is hidden
	_, perr := f.pool.Exec(f.ctx, `UPDATE purchase_plans SET next_execution_date = now() - interval '30 days' WHERE id = $1`, f.planID)
	require.NoError(t, perr)
	_, err = f.tick()
	require.ErrorIs(t, err, errExecutionNotNotifiable)
	rows := f.stepRows(2)
	require.Len(t, rows, 1)
	assert.Equal(t, "failed", rows[0].Status)
	assert.Contains(t, rows[0].Error, "without a frozen baseline skipped")
}

func TestCumulativeAttach_ZeroBuyFanOutCannotAdvance(t *testing.T) {
	f := newCumFixture(t, 100)
	f.src.shown = func() []config.RecommendationRecord {
		return []config.RecommendationRecord{cumRec(f.account, "m5.t0", 100)}
	}
	_, err := f.tick()
	require.NoError(t, err)
	f.buy(1)

	cases := map[string]config.PurchaseExecution{
		"failed fan-out, nothing submitted": {Status: "failed", NoSubmission: true},
		"failed fan-out, unknown outcome":   {Status: "failed"},
		"canceled":                          {Status: "canceled"},
	}
	for name, row := range cases {
		t.Run(name, func(t *testing.T) {
			row.PlanID, row.StepNumber, row.ScheduledDate = f.planID, 2, time.Now()
			row.ExecutionID = fmt.Sprintf("00000000-0000-4000-8000-%012d", len(name))
			require.NoError(t, f.store.SavePurchaseExecution(f.ctx, &row))
			err := f.store.CompletePlanStep(f.ctx, f.planID, 2)
			require.ErrorIs(t, err, config.ErrRampStepIncomplete)
			assert.Equal(t, 1, f.plan().RampSchedule.CurrentStep)
			_, derr := f.pool.Exec(f.ctx, `DELETE FROM purchase_executions WHERE execution_id = $1`, row.ExecutionID)
			require.NoError(t, derr)
		})
	}
}

func TestCumulativeAttach_ConcurrentTicksMintOneRowAndOneBaseline(t *testing.T) {
	f := newCumFixture(t, 100)
	f.src.shown = func() []config.RecommendationRecord {
		return []config.RecommendationRecord{cumRec(f.account, "m5.t0", 100)}
	}
	plan := f.plan()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _, _, _ = f.m.getOrCreateExecution(f.ctx, plan) }()
	}
	wg.Wait()
	rows := f.stepRows(1)
	require.Len(t, rows, 1)
	assert.Equal(t, 20, totalCount(&rows[0]))
	base, err := f.baseline()
	require.NoError(t, err)
	assert.Len(t, base, 1)
}

func TestCumulativeAttach_TargetMetCompletesThePreCreatedRootInPlace(t *testing.T) {
	f := newCumFixture(t, 100)
	f.src.shown = func() []config.RecommendationRecord {
		return []config.RecommendationRecord{cumRec(f.account, "m5.t0", 100)}
	}
	_, err := f.tick()
	require.NoError(t, err)
	f.buy(1)
	held := cumRec(f.account, "m5.t0", 20)
	held.Selected = true
	require.NoError(t, f.store.SavePurchaseExecution(f.ctx, &config.PurchaseExecution{PlanID: f.planID, ExecutionID: "00000000-0000-4000-8000-00000000e001",
		Status: "completed", StepNumber: 5, ScheduledDate: time.Now(), Recommendations: []config.RecommendationRecord{held}}))
	const rootID = "00000000-0000-4000-8000-0000000000a2"
	require.NoError(t, f.store.SavePurchaseExecution(f.ctx, &config.PurchaseExecution{PlanID: f.planID, ExecutionID: rootID,
		Status: "pending", StepNumber: 2, ScheduledDate: time.Now().Add(40 * 24 * time.Hour)}))

	_, err = f.tick()
	require.ErrorIs(t, err, errExecutionNotNotifiable)
	rows := f.stepRows(2)
	require.Len(t, rows, 1, "no second row")
	assert.Equal(t, rootID, rows[0].ExecutionID)
	assert.Equal(t, "completed", rows[0].Status)
	assert.Equal(t, 2, f.plan().RampSchedule.CurrentStep)
}
