package purchase

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/planstep"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Mock store and fake resolver (fixtures). The real-PostgreSQL behavior is in
// plan_step_attach_integration_test.go.

func attachPlan(next time.Time, capAmount float64) *config.PurchasePlan {
	return &config.PurchasePlan{
		ID: "plan-attach", Name: "Attach", Enabled: true, AutoPurchase: true, NotificationDaysBefore: 3,
		MaxStepCommitment: &capAmount, NextExecutionDate: &next,
		Services: map[string]config.ServiceConfig{"azure:compute": {Enabled: true, Provider: "azure", Service: "compute"}},
	}
}

func attachRecs() []config.RecommendationRecord {
	m := 10.0
	acct := "acct-1"
	return []config.RecommendationRecord{
		{ID: "rec-b", Provider: "azure", Service: "compute", Region: "westeurope", ResourceType: "D2", Count: 1, Term: 1, MonthlyCost: &m, CloudAccountID: &acct, Savings: 1},
		{ID: "rec-a", Provider: "azure", Service: "compute", Region: "westeurope", ResourceType: "D4", Count: 2, Term: 1, MonthlyCost: &m, CloudAccountID: &acct, Savings: 2},
	}
}

func attachManager(t *testing.T, listed, locked *config.PurchasePlan, res *fakeResolver, rows ...config.PurchaseExecution) (*Manager, *MockConfigStore) {
	t.Helper()
	store := new(MockConfigStore)
	store.On("LockPurchasePlanTx", mock.Anything, mock.Anything, listed.ID).Return(locked, nil)
	store.On("ListExecutionsForPlanStepTx", mock.Anything, mock.Anything, listed.ID, 1).Return(rows, nil)
	store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{}, nil)
	return &Manager{config: store, resolver: res}, store
}

func savedRows(store *MockConfigStore) []*config.PurchaseExecution {
	var out []*config.PurchaseExecution
	for _, c := range store.Calls {
		if c.Method == "SavePurchaseExecution" {
			out = append(out, c.Arguments.Get(1).(*config.PurchaseExecution))
		}
	}
	return out
}

func TestAttach_NewRowIsPendingWithoutTokenAndRotatesIn(t *testing.T) {
	plan := attachPlan(time.Now().Add(48*time.Hour), 1e6)
	res := &fakeResolver{recs: attachRecs()}
	m, store := attachManager(t, plan, plan, res)
	store.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(nil)
	store.On("CreateSuppressionTx", mock.Anything, mock.Anything, mock.Anything).Return(nil)

	exec, raw, rotate, err := m.getOrCreateExecution(context.Background(), plan)
	require.NoError(t, err)
	require.True(t, rotate, "a new row must take the rotate-after-email path so it becomes notified")
	assert.NotEmpty(t, raw)
	saved := savedRows(store)
	require.Len(t, saved, 1)
	assert.Equal(t, "pending", saved[0].Status)
	assert.NotEmpty(t, saved[0].IdempotencyKey)
	require.Len(t, exec.Recommendations, 2)
	assert.Equal(t, "rec-a", exec.Recommendations[0].ID, "recommendations are persisted sorted so a re-drive derives the same tokens")
	assert.Equal(t, 1, int(res.calls.Load()))
	store.AssertNumberOfCalls(t, "CreateSuppressionTx", 2)
}

func TestAttach_NoMatchWaitsForGraceThenFailsOnce(t *testing.T) {
	for _, tc := range []struct {
		name     string
		next     time.Time
		wantFail bool
	}{
		{"before the deadline", time.Now().Add(-6 * 24 * time.Hour), false},
		{"after the deadline", time.Now().Add(-8 * 24 * time.Hour), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := attachPlan(tc.next, 1e6)
			m, store := attachManager(t, plan, plan, &fakeResolver{err: planstep.ErrNoMatch})
			store.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(nil)

			_, _, _, err := m.getOrCreateExecution(context.Background(), plan)
			require.ErrorIs(t, err, errExecutionNotNotifiable)
			saved := savedRows(store)
			if !tc.wantFail {
				assert.Empty(t, saved, "ErrNoMatch inside the grace window writes no row")
				return
			}
			require.Len(t, saved, 1)
			assert.Equal(t, "failed", saved[0].Status)
			assert.Contains(t, saved[0].Error, "no current recommendation matched")
		})
	}
}

func TestAttach_CapIsCheckedOnTheLockedPlan(t *testing.T) {
	listed := attachPlan(time.Now().Add(48*time.Hour), 1e6)
	locked := attachPlan(*listed.NextExecutionDate, 5) // lowered between list and lock
	m, store := attachManager(t, listed, locked, &fakeResolver{recs: attachRecs()})
	store.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(nil)

	_, _, _, err := m.getOrCreateExecution(context.Background(), listed)
	require.ErrorIs(t, err, errExecutionNotNotifiable)
	saved := savedRows(store)
	require.Len(t, saved, 1)
	assert.Equal(t, "failed", saved[0].Status)
	assert.Empty(t, saved[0].Recommendations)
	assert.Contains(t, saved[0].Error, "exceeds max_step_commitment 5.00")
	assert.Contains(t, saved[0].Error, "rec-a, rec-b")
}

func TestAttach_RefusesWithoutVetoWindowBeforeResolving(t *testing.T) {
	plan := attachPlan(time.Now().Add(48*time.Hour), 1e6)
	plan.NotificationDaysBefore = 0
	res := &fakeResolver{recs: attachRecs()}
	m, store := attachManager(t, plan, plan, res)

	_, _, _, err := m.getOrCreateExecution(context.Background(), plan)
	require.ErrorIs(t, err, config.ErrNoVetoWindow)
	assert.Zero(t, res.calls.Load())
	assert.Empty(t, savedRows(store))
}

func TestAttach_PlanChangedSinceListedSkipsQuietly(t *testing.T) {
	listed := attachPlan(time.Now().Add(48*time.Hour), 1e6)
	for name, change := range map[string]func(*config.PurchasePlan){
		"disabled":   func(p *config.PurchasePlan) { p.Enabled = false },
		"auto off":   func(p *config.PurchasePlan) { p.AutoPurchase = false },
		"date moved": func(p *config.PurchasePlan) { d := p.NextExecutionDate.Add(time.Hour); p.NextExecutionDate = &d },
		"service moved": func(p *config.PurchasePlan) {
			p.Services["azure:compute"] = config.ServiceConfig{Enabled: true, Provider: "aws"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			locked := attachPlan(*listed.NextExecutionDate, 1e6)
			change(locked)
			res := &fakeResolver{recs: attachRecs()}
			m, store := attachManager(t, listed, locked, res)
			_, _, _, err := m.getOrCreateExecution(context.Background(), listed)
			require.ErrorIs(t, err, errExecutionNotNotifiable)
			assert.Zero(t, res.calls.Load())
			assert.Empty(t, savedRows(store))
		})
	}
}

func TestAttach_PreCreatedEmptyRootRow(t *testing.T) {
	plan := attachPlan(time.Now().Add(48*time.Hour), 1e6)
	root := config.PurchaseExecution{ExecutionID: "root", PlanID: plan.ID, Status: "pending", StepNumber: 1, ScheduledDate: time.Now().Add(10 * 24 * time.Hour)}

	t.Run("attaches in place without a second row", func(t *testing.T) {
		m, store := attachManager(t, plan, plan, &fakeResolver{recs: attachRecs()}, root)
		store.On("SetExecutionRecommendationsIfEmptyTx", mock.Anything, mock.Anything, "root", mock.Anything, mock.Anything, mock.Anything).Return(true, nil)
		exec, _, rotate, err := m.getOrCreateExecution(context.Background(), plan)
		require.NoError(t, err)
		assert.True(t, rotate)
		assert.Equal(t, "root", exec.ExecutionID)
		assert.Empty(t, savedRows(store))
	})
	t.Run("an empty row due inside the veto window is left to the scheduler", func(t *testing.T) {
		due := root
		due.ScheduledDate = time.Now().Add(time.Hour) // plan NotificationDaysBefore is 3
		res := &fakeResolver{recs: attachRecs()}
		m, _ := attachManager(t, plan, plan, res, due)
		_, _, _, err := m.getOrCreateExecution(context.Background(), plan)
		require.ErrorIs(t, err, errExecutionNotNotifiable)
		assert.Zero(t, res.calls.Load())
	})
}

func TestAttachScheduledDate_NeverInsideTheVetoWindow(t *testing.T) {
	now := time.Now()
	plan := func(d time.Duration) *config.PurchasePlan {
		next := now.Add(d)
		return &config.PurchasePlan{NotificationDaysBefore: 30, NextExecutionDate: &next}
	}
	windowEnd := now.AddDate(0, 0, 30)
	assert.WithinDuration(t, windowEnd, attachScheduledDate(plan(-48*time.Hour), now), time.Second, "date past")
	assert.WithinDuration(t, windowEnd, attachScheduledDate(plan(10*time.Minute), now), time.Second, "date in the future but inside the window")
	beyond := plan(45 * 24 * time.Hour)
	assert.Equal(t, *beyond.NextExecutionDate, attachScheduledDate(beyond, now), "date beyond the window is unchanged")
}

func TestAttach_NewRowGetsTheVetoWindowWhenThePlanDateIsClose(t *testing.T) {
	plan := attachPlan(time.Now().Add(10*time.Minute), 1e6) // NotificationDaysBefore = 3
	m, store := attachManager(t, plan, plan, &fakeResolver{recs: attachRecs()})
	store.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(nil)
	store.On("CreateSuppressionTx", mock.Anything, mock.Anything, mock.Anything).Return(nil)

	exec, _, _, err := m.getOrCreateExecution(context.Background(), plan)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().AddDate(0, 0, 3), exec.ScheduledDate, time.Minute)
	assert.WithinDuration(t, time.Now().AddDate(0, 0, 3), savedRows(store)[0].ScheduledDate, time.Minute)
}

// An overdue plan whose step already has a recommendation-bearing row is not
// notified again (the plan date has passed; the scheduler owns the row).
func TestAttach_OverduePlanWithRecommendationBearingRowIsSkipped(t *testing.T) {
	plan := attachPlan(time.Now().Add(-time.Hour), 1e6)
	row := config.PurchaseExecution{ExecutionID: "has-recs", PlanID: plan.ID, Status: "notified", StepNumber: 1, Recommendations: attachRecs()}
	m, _ := attachManager(t, plan, plan, &fakeResolver{recs: attachRecs()}, row)

	exec, _, _, err := m.getOrCreateExecution(context.Background(), plan)
	require.ErrorIs(t, err, errExecutionNotNotifiable)
	assert.Nil(t, exec)
}

func TestShouldRetryOverduePlan_Window(t *testing.T) {
	now := time.Now()
	global := &config.GlobalConfig{}
	at := func(d time.Duration) config.PurchasePlan {
		p := attachPlan(now.Add(d), 1)
		return *p
	}
	assert.True(t, shouldRetryOverduePlan(at(-time.Hour), true, global, now))
	assert.True(t, shouldRetryOverduePlan(at(-7*24*time.Hour-time.Hour), true, global, now), "the first tick past the deadline must run to write the failed row")
	assert.False(t, shouldRetryOverduePlan(at(-9*24*time.Hour), true, global, now), "a long-stale plan is not revived")
	assert.False(t, shouldRetryOverduePlan(at(time.Hour), true, global, now), "a future plan is the normal path")
	assert.False(t, shouldRetryOverduePlan(at(-time.Hour), false, global, now), "kill switch off")
}

func TestPlanUnchanged_ComparesDatesAtMicrosecondPrecision(t *testing.T) {
	base := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	withNanos := base.Add(789 * time.Nanosecond)
	listed := attachPlan(withNanos, 1)
	locked := attachPlan(base, 1)
	providers := planProviders(locked)
	assert.True(t, planUnchanged(listed, locked, providers), "sub-microsecond difference is not a change")
	moved := base.Add(time.Microsecond)
	locked.NextExecutionDate = &moved
	assert.False(t, planUnchanged(listed, locked, providers), "a microsecond is a change")
}

// An already-armed multi-step Savings Plans plan (platform#631 Q7) must produce
// one visible failed row with the reason, not an error on every tick.
func TestAttach_UnsupportedPlanFailsTheStepVisibly(t *testing.T) {
	listed := attachPlan(time.Now().Add(48*time.Hour), 1e6)
	locked := attachPlan(*listed.NextExecutionDate, 1e6)
	for _, p := range []*config.PurchasePlan{listed, locked} {
		p.Services = map[string]config.ServiceConfig{"aws/savingsplans": {Enabled: true, Provider: "aws", Service: "savingsplans"}}
		p.RampSchedule = config.RampSchedule{Type: "weekly", PercentPerStep: 25, TotalSteps: 4}
	}
	res := &fakeResolver{err: planstep.Precheck(locked)}
	require.ErrorIs(t, res.err, planstep.ErrUnsupportedPlan)
	m, store := attachManager(t, listed, locked, res)
	store.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(nil)

	_, _, _, err := m.getOrCreateExecution(context.Background(), listed)
	require.ErrorIs(t, err, errExecutionNotNotifiable)
	saved := savedRows(store)
	require.Len(t, saved, 1)
	assert.Equal(t, "failed", saved[0].Status)
	assert.Contains(t, saved[0].Error, "multi-step ramps do not support Savings Plans")
}
