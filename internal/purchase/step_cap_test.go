package purchase

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// All fixtures below are mocks (MockConfigStore) and hand-built records, not
// real cloud data.

func f64(v float64) *float64 { return &v }

func upfrontRec(upfront float64) config.RecommendationRecord {
	return config.RecommendationRecord{ID: "r-upfront", Provider: "aws", Service: "ec2", Count: 1, Term: 1, UpfrontCost: upfront}
}

// azureMonthlyRec has upfront 0 and only a monthly charge, the shape that makes
// an upfront-only cap blind.
func azureMonthlyRec(monthly float64, termYears int) config.RecommendationRecord {
	return config.RecommendationRecord{ID: "r-monthly", Provider: "azure", Service: "compute", Count: 1, Term: termYears, UpfrontCost: 0, MonthlyCost: f64(monthly)}
}

func TestCheckStepCommitmentCap(t *testing.T) {
	cases := []struct {
		name    string
		plan    *config.PurchasePlan
		recs    []config.RecommendationRecord
		wantErr bool
	}{
		{"NULL cap refuses", &config.PurchasePlan{}, []config.RecommendationRecord{upfrontRec(10)}, true},
		{"nil plan refuses", nil, []config.RecommendationRecord{upfrontRec(10)}, true},
		{"zero cap refuses", &config.PurchasePlan{MaxStepCommitment: f64(0)}, []config.RecommendationRecord{upfrontRec(10)}, true},
		{"negative cap refuses", &config.PurchasePlan{MaxStepCommitment: f64(-1)}, []config.RecommendationRecord{upfrontRec(10)}, true},
		{"NaN cap refuses", &config.PurchasePlan{MaxStepCommitment: f64(math.NaN())}, []config.RecommendationRecord{upfrontRec(10)}, true},
		{"Inf cap refuses", &config.PurchasePlan{MaxStepCommitment: f64(math.Inf(1))}, []config.RecommendationRecord{upfrontRec(10)}, true},
		{"over cap refuses", &config.PurchasePlan{MaxStepCommitment: f64(99)}, []config.RecommendationRecord{upfrontRec(100)}, true},
		{"sum of recs over cap refuses", &config.PurchasePlan{MaxStepCommitment: f64(100)}, []config.RecommendationRecord{upfrontRec(60), upfrontRec(60)}, true},
		{"zero commitment refuses", &config.PurchasePlan{MaxStepCommitment: f64(1000)}, []config.RecommendationRecord{upfrontRec(0)}, true},
		{"zero rec among good refuses", &config.PurchasePlan{MaxStepCommitment: f64(1000)}, []config.RecommendationRecord{upfrontRec(10), upfrontRec(0)}, true},
		{"negative commitment refuses", &config.PurchasePlan{MaxStepCommitment: f64(1000)}, []config.RecommendationRecord{upfrontRec(-5)}, true},
		{"NaN commitment refuses", &config.PurchasePlan{MaxStepCommitment: f64(1000)}, []config.RecommendationRecord{upfrontRec(math.NaN())}, true},
		{"azure monthly counted on total not upfront", &config.PurchasePlan{MaxStepCommitment: f64(1000)}, []config.RecommendationRecord{azureMonthlyRec(100, 1)}, true}, // 100*12=1200 > 1000
		{"azure monthly under cap passes", &config.PurchasePlan{MaxStepCommitment: f64(1200)}, []config.RecommendationRecord{azureMonthlyRec(100, 1)}, false},
		{"exactly at cap passes", &config.PurchasePlan{MaxStepCommitment: f64(100)}, []config.RecommendationRecord{upfrontRec(100)}, false},
		{"no recs is not this check's concern", &config.PurchasePlan{}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckStepCommitmentCap(tc.plan, tc.recs)
			if tc.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrStepCommitmentCap)
				return
			}
			assert.NoError(t, err)
		})
	}
}

// gateHarness drives one due plan row through ProcessScheduledPurchases with a
// mock store. TransitionExecutionStatus is deliberately NOT mocked: a claim
// attempt (i.e. anything that could go on to buy) panics the mock and fails
// the test.
func gateHarness(t *testing.T, plan *config.PurchasePlan, exec config.PurchaseExecution, cfg *config.GlobalConfig, cfgErr error) *ProcessResult {
	t.Helper()
	ctx := context.Background()
	store := new(MockConfigStore)
	store.On("GetStaleApprovedExecutions", ctx, mock.Anything).Return([]config.PurchaseExecution{}, nil)
	store.On("GetPendingExecutions", ctx).Return([]config.PurchaseExecution{exec}, nil)
	store.On("GetPurchasePlan", ctx, exec.PlanID).Return(plan, nil)
	store.On("GetGlobalConfig", ctx).Return(cfg, cfgErr)
	manager := &Manager{config: store, email: new(MockEmailSender)}
	result, err := manager.ProcessScheduledPurchases(ctx)
	require.NoError(t, err)
	store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	return result
}

func dueRecExec(status string, recs ...config.RecommendationRecord) config.PurchaseExecution {
	return config.PurchaseExecution{
		ExecutionID: "exec-gate", PlanID: "plan-gate", Status: status, StepNumber: 1,
		ScheduledDate: time.Now().Add(-time.Hour), Recommendations: recs,
	}
}

func TestScheduler_RefusesBeforeClaim(t *testing.T) {
	capped := func(c *float64) *config.PurchasePlan {
		return &config.PurchasePlan{ID: "plan-gate", AutoPurchase: true, MaxStepCommitment: c}
	}
	cases := []struct {
		name       string
		plan       *config.PurchasePlan
		exec       config.PurchaseExecution
		cfg        *config.GlobalConfig
		cfgErr     error
		wantFailed int // 0 = silently skipped, 1 = surfaced as a failure
	}{
		{"kill switch off", capped(f64(1000)), dueRecExec("notified", upfrontRec(10)), &config.GlobalConfig{}, nil, 0},
		{"kill switch read error", capped(f64(1000)), dueRecExec("notified", upfrontRec(10)), nil, errors.New("db down"), 1},
		{"read error even with a populated config", capped(f64(1000)), dueRecExec("notified", upfrontRec(10)), autoPurchaseOnCfg(), errors.New("db down"), 1},
		{"nil global config", capped(f64(1000)), dueRecExec("notified", upfrontRec(10)), nil, nil, 1},
		{"pending rec-bearing row (no notice sent)", capped(f64(1000)), dueRecExec("pending", upfrontRec(10)), autoPurchaseOnCfg(), nil, 0},
		{"NULL cap", capped(nil), dueRecExec("notified", upfrontRec(10)), autoPurchaseOnCfg(), nil, 1},
		{"over cap", capped(f64(5)), dueRecExec("notified", upfrontRec(10)), autoPurchaseOnCfg(), nil, 1},
		{"zero commitment", capped(f64(1000)), dueRecExec("notified", upfrontRec(0)), autoPurchaseOnCfg(), nil, 1},
		// The store interface permits (nil, nil); the gate must refuse, not panic (A05-013).
		{"plan missing", nil, dueRecExec("notified", upfrontRec(10)), autoPurchaseOnCfg(), nil, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := gateHarness(t, tc.plan, tc.exec, tc.cfg, tc.cfgErr)
			assert.Equal(t, 0, result.Processed)
			assert.Equal(t, 0, result.Executed)
			assert.Equal(t, tc.wantFailed, result.Failed)
		})
	}
}

// The SQS execute_purchase path enforces the same refusals before the claim.
func TestSQSExecutePath_RefusesBeforeClaim(t *testing.T) {
	capped := func(c *float64) *config.PurchasePlan {
		return &config.PurchasePlan{ID: "plan-gate", AutoPurchase: true, MaxStepCommitment: c}
	}
	cases := []struct {
		name   string
		plan   *config.PurchasePlan
		exec   config.PurchaseExecution
		cfg    *config.GlobalConfig
		cfgErr error
	}{
		{"kill switch off", capped(f64(1000)), dueRecExec("notified", upfrontRec(10)), &config.GlobalConfig{}, nil},
		{"kill switch read error", capped(f64(1000)), dueRecExec("notified", upfrontRec(10)), nil, errors.New("db down")},
		{"pending rec-bearing row", capped(f64(1000)), dueRecExec("pending", upfrontRec(10)), autoPurchaseOnCfg(), nil},
		{"NULL cap", capped(nil), dueRecExec("notified", upfrontRec(10)), autoPurchaseOnCfg(), nil},
		{"over cap", capped(f64(5)), dueRecExec("notified", upfrontRec(10)), autoPurchaseOnCfg(), nil},
		{"zero commitment", capped(f64(1000)), dueRecExec("notified", upfrontRec(0)), autoPurchaseOnCfg(), nil},
		{"plan missing", nil, dueRecExec("notified", upfrontRec(10)), autoPurchaseOnCfg(), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := new(MockConfigStore)
			exec := tc.exec
			store.On("GetExecutionByID", ctx, exec.ExecutionID).Return(&exec, nil)
			store.On("GetPurchasePlan", ctx, exec.PlanID).Return(tc.plan, nil)
			store.On("GetGlobalConfig", ctx).Return(tc.cfg, tc.cfgErr)
			manager := &Manager{config: store, email: new(MockEmailSender)}

			err := manager.handleExecutePurchase(ctx, &AsyncMessage{Type: MessageTypeExecutePurchase, ExecutionID: exec.ExecutionID})

			require.Error(t, err, "the message must be dead-lettered, not acked as done")
			store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

// Manual purchases are not touched by the kill switch or the cap: an approved
// row (human approval path) bypasses the auto-execute gate entirely, and the
// gate never reads the global config for it.
func TestAutoExecuteGate_ApprovedRowIgnoresKillSwitchAndCap(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore) // no GetGlobalConfig / GetPurchasePlan expectation: any read panics
	manager := &Manager{config: store, email: new(MockEmailSender)}
	exec := dueRecExec("approved", upfrontRec(1e9))
	require.NoError(t, manager.checkAutoExecuteGate(ctx, &exec))
	store.AssertNotCalled(t, "GetGlobalConfig", mock.Anything)
	store.AssertNotCalled(t, "GetPurchasePlan", mock.Anything, mock.Anything)
}

// A web-submitted direct purchase (no plan) never reaches the plan gate.
func TestExecutableByScheduler_WebRowNeverReadsKillSwitch(t *testing.T) {
	store := new(MockConfigStore)
	manager := &Manager{config: store}
	exec := config.PurchaseExecution{ExecutionID: "w", Status: "pending", Source: "cudly-web", Recommendations: []config.RecommendationRecord{upfrontRec(1)}}
	ok, err := manager.executableByScheduler(context.Background(), &exec)
	require.NoError(t, err)
	assert.False(t, ok)
	store.AssertNotCalled(t, "GetGlobalConfig", mock.Anything)
}

// An AutoPurchase=false plan is skipped without consulting the kill switch.
func TestExecutableByScheduler_NonAutoPlanNeverReadsKillSwitch(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	store.On("GetPurchasePlan", ctx, "plan-gate").Return(&config.PurchasePlan{ID: "plan-gate"}, nil)
	manager := &Manager{config: store}
	exec := dueRecExec("notified", upfrontRec(1))
	ok, err := manager.executableByScheduler(ctx, &exec)
	require.NoError(t, err)
	assert.False(t, ok)
	store.AssertNotCalled(t, "GetGlobalConfig", mock.Anything)
}

func TestShouldNotifyPlan_KillSwitch(t *testing.T) {
	next := time.Now().Add(24 * time.Hour)
	plan := config.PurchasePlan{Enabled: true, AutoPurchase: true, NotificationDaysBefore: 7, NextExecutionDate: &next}
	m := &Manager{}
	assert.True(t, m.shouldNotifyPlan(plan, true))
	assert.False(t, m.shouldNotifyPlan(plan, false), "kill switch off must suppress the notification")
}

func TestSendUpcomingPurchaseNotifications_KillSwitch(t *testing.T) {
	next := time.Now().Add(24 * time.Hour)
	plan := config.PurchasePlan{ID: "p", Enabled: true, AutoPurchase: true, NotificationDaysBefore: 7, NextExecutionDate: &next}

	t.Run("off sends nothing and touches no plan", func(t *testing.T) {
		ctx := context.Background()
		store := new(MockConfigStore)
		store.On("ListPurchasePlans", ctx, config.PurchasePlanFilter{}).Return([]config.PurchasePlan{plan}, nil)
		store.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{}, nil)
		manager := &Manager{config: store, email: new(MockEmailSender)}
		res, err := manager.SendUpcomingPurchaseNotifications(ctx)
		require.NoError(t, err)
		assert.Equal(t, 0, res.Notified)
		store.AssertNotCalled(t, "LockPurchasePlanTx", mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("read error fails the tick", func(t *testing.T) {
		ctx := context.Background()
		store := new(MockConfigStore)
		store.On("ListPurchasePlans", ctx, config.PurchasePlanFilter{}).Return([]config.PurchasePlan{plan}, nil)
		store.On("GetGlobalConfig", ctx).Return(nil, errors.New("db down"))
		manager := &Manager{config: store, email: new(MockEmailSender)}
		_, err := manager.SendUpcomingPurchaseNotifications(ctx)
		require.Error(t, err)
	})
}

// A direct-execute row (no PlanID) left pending is never auto-purchased, and
// the gate must not hand "" to the store: Postgres rejects it with SQLSTATE
// 22P02, which every tick would count as a failure (issue #243).
func TestScheduler_DirectExecuteRowSkippedWithoutPlanLookup(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	exec := dueRecExec("pending", upfrontRec(10))
	exec.PlanID = ""
	store.On("GetStaleApprovedExecutions", ctx, mock.Anything).Return([]config.PurchaseExecution{}, nil)
	store.On("GetPendingExecutions", ctx).Return([]config.PurchaseExecution{exec}, nil)
	store.On("GetPurchasePlan", ctx, "").Return(nil, errors.New(`ERROR: invalid input syntax for type uuid: "" (SQLSTATE 22P02)`))
	manager := &Manager{config: store, email: new(MockEmailSender)}

	result, err := manager.ProcessScheduledPurchases(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, result.Failed, "a plan-less row must not poison the tick's failure count: %v", result.Errors)
	assert.Equal(t, 0, result.Processed)
	store.AssertNotCalled(t, "GetPurchasePlan", mock.Anything, mock.Anything)
	store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}
