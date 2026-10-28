//go:build integration

package purchase

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

type b3C1Root struct {
	present bool
	offset  time.Duration
}

type b3C1Snapshot struct {
	plan                          *config.PurchasePlan
	rows                          [3][]config.PurchaseExecution
	rawSteps                      map[int][]json.RawMessage
	rawPlan, rawRows, rawAccounts []byte
	baseline                      []byte
	ledger                        string
	suppressed                    int
	rawSuppressions               []byte
	suppressions                  []config.PurchaseSuppression
}

type b3C1Fixture struct {
	*cumFixture
	now, formula  time.Time
	calls         atomic.Int32
	sender        *countingSender
	before, after b3C1Snapshot
}

func b3C1Read(f *cumFixture) b3C1Snapshot {
	f.t.Helper()
	s := b3C1Snapshot{plan: f.plan()}
	for step := 1; step <= 3; step++ {
		s.rows[step-1] = f.stepRows(step)
	}
	require.NoError(f.t, f.pool.QueryRow(f.ctx, `SELECT to_jsonb(p), ramp_baseline, ramp_ledger_state FROM purchase_plans p WHERE id=$1`, f.planID).Scan(&s.rawPlan, &s.baseline, &s.ledger))
	require.NoError(f.t, f.pool.QueryRow(f.ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(e) ORDER BY step_number, execution_id), '[]'::jsonb) FROM purchase_executions e WHERE plan_id=$1`, f.planID).Scan(&s.rawRows))
	var rawRows []json.RawMessage
	require.NoError(f.t, json.Unmarshal(s.rawRows, &rawRows))
	s.rawSteps = make(map[int][]json.RawMessage)
	for _, row := range rawRows {
		var header struct {
			Step int `json:"step_number"`
		}
		require.NoError(f.t, json.Unmarshal(row, &header))
		s.rawSteps[header.Step] = append(s.rawSteps[header.Step], row)
	}
	require.NoError(f.t, f.pool.QueryRow(f.ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(a) ORDER BY account_id), '[]'::jsonb) FROM plan_accounts a WHERE plan_id=$1`, f.planID).Scan(&s.rawAccounts))
	require.NoError(f.t, f.pool.QueryRow(f.ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(s) ORDER BY id), '[]'::jsonb), COALESCE(SUM(suppressed_count),0) FROM purchase_suppressions s WHERE account_id=$1`, f.account).Scan(&s.rawSuppressions, &s.suppressed))
	require.NoError(f.t, json.Unmarshal(s.rawSuppressions, &s.suppressions))
	for _, suppression := range s.suppressions {
		require.Len(f.t, s.rows[2], 1)
		require.Equal(f.t, s.rows[2][0].ExecutionID, suppression.ExecutionID)
		require.Equal(f.t, f.account, suppression.AccountID)
		require.NotEmpty(f.t, suppression.ID)
		require.False(f.t, suppression.CreatedAt.IsZero())
		require.True(f.t, suppression.ExpiresAt.After(suppression.CreatedAt))
		require.Equal(f.t, config.PurchaseSuppression{ID: suppression.ID, ExecutionID: s.rows[2][0].ExecutionID, AccountID: f.account, Provider: "aws", Service: "compute", Region: "us-east-1", ResourceType: "m5.t0", SuppressedCount: 20, CreatedAt: suppression.CreatedAt, ExpiresAt: suppression.ExpiresAt}, suppression)
	}
	return s
}

func newB3C1Fixture(t *testing.T, automatic, targetMet bool, root b3C1Root) *b3C1Fixture {
	t.Helper()
	f := &b3C1Fixture{cumFixture: newCumFixture(t, 100)}
	f.now = time.Now().UTC().Truncate(time.Microsecond)
	f.formula = f.now.Add(84 * time.Hour)
	plan := f.plan()
	plan.AutoPurchase, plan.NotificationDaysBefore = automatic, 3
	plan.RampSchedule.StartDate = f.formula.AddDate(0, 0, -14)
	initial := f.now.Add(60 * time.Hour)
	plan.NextExecutionDate = &initial
	require.NoError(t, f.store.UpdatePurchasePlan(f.ctx, plan))
	fresh := b3C1Read(f.cumFixture)
	require.Equal(t, "new", fresh.ledger)
	require.Nil(t, fresh.baseline)
	require.Empty(t, fresh.rows[0])
	rec := cumRec(f.account, "m5.t0", 100)
	identity, identityErr := config.RecIdentityKey(&rec)
	require.NoError(t, identityErr)
	base := config.PlanRampBaseline{identity: {Count: 100}}
	var wrote bool
	require.NoError(t, f.store.WithTx(f.ctx, func(tx pgx.Tx) error {
		_, err := f.store.LockPurchasePlanTx(f.ctx, tx, f.planID)
		if err != nil {
			return err
		}
		wrote, err = f.store.SetPlanRampBaselineTx(f.ctx, tx, f.planID, base)
		return err
	}))
	require.True(t, wrote)
	trusted := b3C1Read(f.cumFixture)
	require.Equal(t, "trusted", trusted.ledger)
	var storedBase config.PlanRampBaseline
	require.NoError(t, json.Unmarshal(trusted.baseline, &storedBase))
	require.Equal(t, base, storedBase)
	accounts, err := f.store.GetPlanAccounts(f.ctx, f.planID)
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	require.Equal(t, f.account, accounts[0].ID)
	quantity := 20
	if targetMet {
		quantity = 40
	}
	held := cumRec(f.account, "m5.t0", quantity)
	held.Selected = true
	held.Purchased = true
	// SIMULATED provider completion: selected retained capacity, never a provider call.
	prefix := &config.PurchaseExecution{PlanID: f.planID, ExecutionID: uuid.NewString(), StepNumber: 1,
		Status: "completed", ScheduledDate: f.now.Add(-24 * time.Hour), CompletedAt: &f.now,
		Recommendations: []config.RecommendationRecord{held}}
	require.NoError(t, f.store.SavePurchaseExecution(f.ctx, prefix))
	require.NoError(t, f.store.CompletePlanStep(f.ctx, f.planID, 1))
	require.Equal(t, 1, f.plan().RampSchedule.CurrentStep)
	second := &config.PurchaseExecution{PlanID: f.planID, ExecutionID: uuid.NewString(), StepNumber: 2,
		Status: "pending", ScheduledDate: f.now.Add(24 * time.Hour)}
	if !targetMet {
		second.Status, second.CompletedAt = "completed", &f.now
		second.Recommendations = []config.RecommendationRecord{held}
	}
	require.NoError(t, f.store.SavePurchaseExecution(f.ctx, second))
	if root.present {
		next := &config.PurchaseExecution{PlanID: f.planID, ExecutionID: uuid.NewString(), StepNumber: 3,
			Status: "pending", ScheduledDate: f.now.Add(root.offset)}
		require.NoError(t, f.store.SavePurchaseExecution(f.ctx, next))
	}
	f.before = b3C1Read(f.cumFixture)
	require.Equal(t, quantity, totalCount(&f.before.rows[0][0]))
	require.True(t, f.before.rows[0][0].Recommendations[0].Selected)
	require.True(t, f.before.rows[0][0].Recommendations[0].Purchased)
	if targetMet {
		require.Empty(t, f.before.rows[1][0].Recommendations)
		require.NoError(t, f.store.WithTx(f.ctx, func(tx pgx.Tx) error {
			if err := f.store.LockPlanProvidersTx(f.ctx, tx, []string{"aws"}); err != nil {
				return err
			}
			return f.store.CompleteTargetMetStepTx(f.ctx, tx, f.planID, 2, second.ExecutionID, "SIMULATED target already held: 40")
		}))
	} else {
		require.Equal(t, 20, totalCount(&f.before.rows[1][0]))
		require.True(t, f.before.rows[1][0].Recommendations[0].Purchased)
		require.NoError(t, f.store.CompletePlanStep(f.ctx, f.planID, 2))
	}
	f.after = b3C1Read(f.cumFixture)
	require.Equal(t, 2, f.after.plan.RampSchedule.CurrentStep)
	require.NotNil(t, f.after.plan.NextExecutionDate)
	require.True(t, f.formula.Equal(*f.after.plan.NextExecutionDate), "characterization: current interval persistence overwrites the plan clock")
	require.Equal(t, f.before.rows[0], f.after.rows[0])
	require.Equal(t, f.before.rawSteps[1], f.after.rawSteps[1])
	require.Equal(t, f.before.rows[2], f.after.rows[2], "completion preserves the existing step3 date/payload")
	require.Equal(t, f.before.rawSteps[3], f.after.rawSteps[3])
	require.Equal(t, trusted.baseline, f.after.baseline)
	require.Equal(t, trusted.rawAccounts, f.after.rawAccounts)
	require.Equal(t, "trusted", f.after.ledger)
	require.Equal(t, "completed", f.after.rows[1][0].Status)
	require.NotNil(t, f.after.rows[1][0].CompletedAt)
	require.Equal(t, f.before.rawSuppressions, f.after.rawSuppressions)
	if targetMet {
		require.NotNil(t, f.after.rows[1][0].PreApprovalSkipReason)
		require.Equal(t, config.PreApprovalSkipRampTargetMet, *f.after.rows[1][0].PreApprovalSkipReason)
		require.Empty(t, f.after.rows[1][0].Recommendations)
	} else {
		require.Equal(t, f.before.rows[1], f.after.rows[1])
		require.Equal(t, f.before.rawSteps[2], f.after.rawSteps[2])
	}
	require.ErrorIs(t, f.store.CompletePlanStep(f.ctx, f.planID, 2), config.ErrRampStepCountedBySibling)
	require.Equal(t, f.after, b3C1Read(f.cumFixture), "duplicate completion writes nothing")
	f.src.shown = func() []config.RecommendationRecord {
		f.calls.Add(1)
		return []config.RecommendationRecord{cumRec(f.account, "m5.t0", 100)}
	}
	f.sender = newCountingSender(f.ctx)
	f.m.email = f.sender
	require.Nil(t, f.m.providerFactory, "notification-only fixture has no provider executor")
	return f
}

func TestB3C1CompletionNotification(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		for _, targetMet := range []bool{false, true} {
			for _, root := range []struct {
				name string
				b3C1Root
			}{
				{"equal_formula", b3C1Root{present: true, offset: 84 * time.Hour}},
				{"earlier", b3C1Root{present: true, offset: 78 * time.Hour}},
				{"later", b3C1Root{present: true, offset: 90 * time.Hour}},
				{"no_root", b3C1Root{}},
			} {
				t.Run(fmt.Sprintf("automatic_%t/target_met_%t/%s", automatic, targetMet, root.name), func(t *testing.T) {
					f := newB3C1Fixture(t, automatic, targetMet, root.b3C1Root)
					before := time.Now()
					require.True(t, f.m.shouldNotifyPlan(*f.after.plan, true))
					result, err := f.m.SendUpcomingPurchaseNotifications(f.ctx)
					require.NoError(t, err)
					after := time.Now()
					for _, clock := range []time.Time{before, after} {
						require.Greater(t, f.formula.Sub(clock).Hours(), 72.0, "inconclusive notification bracket")
						require.Less(t, f.formula.Sub(clock).Hours(), 96.0, "inconclusive notification bracket")
					}
					state := b3C1Read(f.cumFixture)
					want := 0
					if automatic || root.present {
						want = 1
						require.Len(t, state.rows[2], 1)
						for _, clock := range []time.Time{before, after} {
							require.False(t, state.rows[2][0].ScheduledDate.Before(vetoWindowEnd(state.plan, clock)), "inconclusive veto bracket")
						}
						require.Equal(t, "notified", state.rows[2][0].Status)
						require.Equal(t, 20, totalCount(&state.rows[2][0]))
						require.True(t, state.rows[2][0].Recommendations[0].Selected)
						require.Equal(t, 20, state.suppressed)
						require.Len(t, state.suppressions, 1)
						if root.present {
							require.Equal(t, f.after.rows[2][0].ExecutionID, state.rows[2][0].ExecutionID)
							require.True(t, f.after.rows[2][0].ScheduledDate.Equal(state.rows[2][0].ScheduledDate))
						} else {
							require.True(t, attachScheduledDate(f.after.plan, before).Equal(state.rows[2][0].ScheduledDate))
						}
						require.NotNil(t, state.plan.LastNotificationSent)
						require.False(t, state.plan.LastNotificationSent.Before(before))
						require.False(t, state.plan.LastNotificationSent.After(after))
						require.False(t, f.m.shouldNotifyPlan(*state.plan, true), "successful stamp enforces cooldown")
					} else {
						require.Empty(t, state.rows[2])
						require.Equal(t, f.after, state)
					}
					f.sender.mu.Lock()
					sends := f.sender.sends
					f.sender.mu.Unlock()
					require.Equal(t, want, result.Notified)
					require.Equal(t, want, sends)
					require.EqualValues(t, want, f.calls.Load())
					require.Equal(t, f.after.rows[0], state.rows[0])
					require.Equal(t, f.after.rawSteps[1], state.rawSteps[1])
					require.Equal(t, f.after.rows[1], state.rows[1])
					require.Equal(t, f.after.rawSteps[2], state.rawSteps[2])
					require.Equal(t, f.after.baseline, state.baseline)
					require.Equal(t, f.after.rawAccounts, state.rawAccounts)
					require.Equal(t, f.after.ledger, state.ledger)
					require.True(t, f.formula.Equal(*state.plan.NextExecutionDate))
					require.Equal(t, 2, state.plan.RampSchedule.CurrentStep)
					again, err := f.m.SendUpcomingPurchaseNotifications(f.ctx)
					require.NoError(t, err)
					require.Zero(t, again.Notified)
					require.Equal(t, state, b3C1Read(f.cumFixture))
					require.EqualValues(t, want, f.calls.Load())
					f.sender.mu.Lock()
					require.Equal(t, want, f.sender.sends)
					f.sender.mu.Unlock()
				})
			}
		}
	}
}
