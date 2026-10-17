//go:build integration

package purchase

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

func TestCumulativeAttach_RetentionKeepsCommittedCapacity(t *testing.T) {
	f := newCumFixture(t, 100)
	p := f.plan()
	p.RampSchedule.Type, p.RampSchedule.StepIntervalDays = "custom", 30
	require.NoError(t, f.store.UpdatePurchasePlan(f.ctx, p))
	f.src.shown = func() []config.RecommendationRecord {
		return []config.RecommendationRecord{cumRec(f.account, "m5.t0", 100)}
	}
	first, err := f.tick()
	require.NoError(t, err)
	require.Equal(t, 20, totalCount(first))
	f.buy(1)
	f.src.shown = func() []config.RecommendationRecord {
		return []config.RecommendationRecord{cumRec(f.account, "m5.t0", 80)}
	}
	before, err := f.baseline()
	require.NoError(t, err)
	require.Len(t, before, 1)
	baselineCount := 0
	for _, q := range before {
		baselineCount = q.Count
		require.Equal(t, 100, q.Count)
	}
	require.NoError(t, f.store.WithTx(f.ctx, func(tx pgx.Tx) error {
		_, updateErr := tx.Exec(f.ctx, `UPDATE purchase_executions SET scheduled_date=NOW()-INTERVAL '31 days' WHERE plan_id=$1 AND step_number=1`, f.planID)
		return updateErr
	}))
	deleted, err := f.store.CleanupOldExecutions(f.ctx, 30)
	require.NoError(t, err)
	require.Zero(t, deleted)
	after, err := f.baseline()
	require.NoError(t, err)
	require.Equal(t, before, after)
	var committed map[string]config.PlanRampQuantity
	require.NoError(t, f.store.WithTx(f.ctx, func(tx pgx.Tx) error {
		_, lockErr := f.store.LockPurchasePlanTx(f.ctx, tx, f.planID)
		if lockErr != nil {
			return lockErr
		}
		committed, err = f.store.ListPlanCommittedTx(f.ctx, tx, f.planID)
		return err
	}))
	heldCount := 0
	for _, q := range committed {
		heldCount = q.Count
		require.Equal(t, 20, q.Count)
	}
	require.Len(t, committed, 1)
	next, err := f.tick()
	require.NoError(t, err)
	require.Equal(t, 20, totalCount(next), "target 40 less the retained first 20")
	t.Logf("synthetic quantities: baseline=%d first=%d retained=%d next_target=40 next_attached=%d", baselineCount, totalCount(first), heldCount, totalCount(next))
}

func TestCumulativeAttach_FreshEditOverlapsInvisibleHeldInsertion(t *testing.T) {
	f := newCumFixture(t, 100)
	tx, err := f.pool.Begin(f.ctx)
	require.NoError(t, err)
	defer tx.Rollback(f.ctx)
	held := cumRec(f.account, "m5.t0", 20)
	held.Selected = true
	require.NoError(t, f.store.SavePurchaseExecutionTx(f.ctx, tx, &config.PurchaseExecution{
		PlanID: f.planID, ExecutionID: uuid.NewString(), Status: "pending", StepNumber: 2,
		ScheduledDate: time.Now().Add(-31 * 24 * time.Hour), TTL: time.Now().Add(-31 * 24 * time.Hour).Unix(),
		Recommendations: []config.RecommendationRecord{held},
	}))
	bounded, cancel := context.WithTimeout(f.ctx, 2*time.Second)
	defer cancel()
	_, err = f.pool.Exec(bounded, `UPDATE purchase_plans SET ramp_schedule=jsonb_set(jsonb_set(ramp_schedule,'{type}','"custom"'),'{step_interval_days}','30') WHERE id=$1`, f.planID)
	require.NoError(t, err, "old writer edits while the new held row is still invisible")
	require.NoError(t, tx.Commit(f.ctx))
	deleted, err := f.store.CleanupOldExecutions(f.ctx, 30)
	require.NoError(t, err)
	require.Zero(t, deleted)
	f.src.shown = func() []config.RecommendationRecord {
		return []config.RecommendationRecord{cumRec(f.account, "m5.t0", 100)}
	}
	_, err = f.tick()
	require.ErrorIs(t, err, errExecutionNotNotifiable)
	rows := f.stepRows(1)
	require.Len(t, rows, 1)
	require.Empty(t, rows[0].Recommendations, "first target20 is already held, so the freeze must not attach another20")
	require.NotNil(t, rows[0].PreApprovalSkipReason)
	require.Equal(t, config.PreApprovalSkipRampTargetMet, *rows[0].PreApprovalSkipReason)
	base, err := f.baseline()
	require.NoError(t, err)
	require.Len(t, base, 1)
}

func TestCumulativeAttach_UntrustedHistoryCannotSizeOrNotify(t *testing.T) {
	cases := []struct {
		name, rootStatus string
		progress, held   int
		clear            bool
	}{
		{name: "all prior history missing", progress: 1},
		{name: "partial prior history missing", progress: 1, held: 10},
		{name: "progressed baseline missing", progress: 1, clear: true},
		{name: "legacy step zero baseline missing", clear: true},
		{name: "nonempty pending root", progress: 1, rootStatus: "pending"},
		{name: "nonempty notified root", progress: 1, rootStatus: "notified"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCumFixture(t, 100)
			rec := cumRec(f.account, "m5.t0", 100)
			key, err := config.RecIdentityKey(&rec)
			require.NoError(t, err)
			raw, err := json.Marshal(config.PlanRampBaseline{key: {Count: 100}})
			require.NoError(t, err)
			_, err = f.pool.Exec(f.ctx, `UPDATE purchase_plans SET ramp_baseline=$2, ramp_schedule=jsonb_set(ramp_schedule,'{current_step}',to_jsonb($3::int)) WHERE id=$1`, f.planID, raw, tc.progress)
			require.NoError(t, err, "old writer freezes without a trust transition")
			if tc.clear {
				_, err = f.pool.Exec(f.ctx, `UPDATE purchase_plans SET ramp_baseline=NULL WHERE id=$1`, f.planID)
				require.NoError(t, err)
			}
			if tc.held > 0 {
				rec.Count, rec.Selected = tc.held, true
				require.NoError(t, f.store.SavePurchaseExecution(f.ctx, &config.PurchaseExecution{PlanID: f.planID, ExecutionID: uuid.NewString(), Status: config.StatusCompleted, StepNumber: 1, ScheduledDate: time.Now(), Recommendations: []config.RecommendationRecord{rec}}))
			}
			var rootID string
			if tc.rootStatus != "" {
				rec.Selected = true
				rootID = uuid.NewString()
				require.NoError(t, f.store.SavePurchaseExecution(f.ctx, &config.PurchaseExecution{PlanID: f.planID, ExecutionID: rootID, Status: tc.rootStatus, StepNumber: tc.progress + 1, ScheduledDate: time.Now().Add(40 * 24 * time.Hour), Recommendations: []config.RecommendationRecord{rec}}))
				for _, id := range []string{rootID} {
					require.NoError(t, f.store.CreateSuppression(f.ctx, &config.PurchaseSuppression{ExecutionID: id, AccountID: "sup-acct", Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.t0", SuppressedCount: 1, ExpiresAt: time.Now().Add(time.Hour)}))
				}
				require.NoError(t, f.store.CreateSuppression(f.ctx, &config.PurchaseSuppression{ExecutionID: uuid.NewString(), AccountID: "other-acct", Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.t0", SuppressedCount: 1, ExpiresAt: time.Now().Add(time.Hour)}))
			}
			calls := 0
			f.src.shown = func() []config.RecommendationRecord { calls++; return []config.RecommendationRecord{rec} }
			_, err = f.tick()
			require.Error(t, err)
			rows := f.stepRows(tc.progress + 1)
			require.Len(t, rows, 1)
			if tc.rootStatus == "" {
				require.Equal(t, config.StatusFailed, rows[0].Status)
				require.Contains(t, rows[0].Error, "cannot be verified")
				require.Empty(t, rows[0].Recommendations)
			} else {
				require.ErrorIs(t, err, errExecutionNotNotifiable, "a durable refusal is a quiet skip, not a raw error")
				require.NotErrorIs(t, err, config.ErrRampLedgerUntrusted)
				require.Equal(t, config.StatusFailed, rows[0].Status)
				require.True(t, rows[0].NoSubmission)
				require.Contains(t, rows[0].Error, config.ErrRampLedgerUntrusted.Error())
				require.NotEmpty(t, rows[0].Recommendations, "the attached recommendations stay on the failed row")
				var released, others int
				require.NoError(t, f.pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM purchase_suppressions WHERE execution_id = $1`, rootID).Scan(&released))
				require.Zero(t, released, "a proven not-submitted failure releases its suppressions")
				require.NoError(t, f.pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM purchase_suppressions WHERE account_id = 'other-acct'`).Scan(&others))
				require.Equal(t, 1, others, "another execution's suppressions are untouched")
			}
			require.False(t, f.m.sendPlanNotification(f.ctx, f.plan()))
			require.Zero(t, calls, "no new sizing or resizing")
			require.Zero(t, f.m.email.(*countingSender).sends)
			require.Zero(t, suppressedTotal(f.ctx, t, f.pool))
			require.Empty(t, rows[0].ApprovalToken)
		})
	}
}

func TestCumulativeAttach_AuthorizedLegacyQuantityRemainsImmutable(t *testing.T) {
	for _, status := range []string{"scheduled", "approved"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			f := newRampStepFixture(ctx, t)
			require.NoError(t, f.store.SetPlanAccounts(ctx, f.plan.ID, []string{f.accounts[0].ID}))
			root := f.saveRootExecution(ctx, t, 3)
			root.Status = status
			root.CloudAccountID = &f.accounts[0].ID
			root.Recommendations = root.Recommendations[:1]
			root.Recommendations[0].Count = 7
			due := time.Now().Add(-time.Minute)
			root.ScheduledExecutionAt = &due
			require.NoError(t, f.store.SavePurchaseExecution(ctx, root))
			if status == "scheduled" {
				result, fireErr := f.manager.FireScheduledDelayedPurchases(ctx)
				require.NoError(t, fireErr)
				require.Equal(t, 1, result.Fired)
				require.Zero(t, result.Errored)
			} else {
				require.NoError(t, f.execute(ctx, root))
			}
			f.mu.Lock()
			counts := append([]int(nil), f.counts...)
			f.mu.Unlock()
			require.Equal(t, []int{7}, counts, "the recording provider receives the authorized frozen quantity")
			resolver := &fakeResolver{recs: root.Recommendations}
			f.manager.resolver = resolver
			err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
				p, lockErr := f.store.LockPurchasePlanTx(ctx, tx, f.plan.ID)
				if lockErr != nil {
					return lockErr
				}
				_, resolveErr := f.manager.resolveCumulative(ctx, tx, p, p.RampSchedule.CurrentStep+1, []string{f.accounts[0].ID})
				return resolveErr
			})
			require.ErrorIs(t, err, config.ErrRampLedgerUntrusted, "the next step may not size from unverifiable history")
			require.Zero(t, resolver.calls.Load())
		})
	}
}

func TestFailExistingPlanStep_LostRaceKeepsSuppressions(t *testing.T) {
	f := newCumFixture(t, 100)
	rec := cumRec(f.account, "m5.t0", 100)
	rec.Selected = true
	root := &config.PurchaseExecution{PlanID: f.planID, ExecutionID: uuid.NewString(), Status: "pending", StepNumber: 1, ScheduledDate: time.Now().Add(40 * 24 * time.Hour), Recommendations: []config.RecommendationRecord{rec}}
	require.NoError(t, f.store.SavePurchaseExecution(f.ctx, root))
	require.NoError(t, f.store.CreateSuppression(f.ctx, &config.PurchaseSuppression{ExecutionID: root.ExecutionID, AccountID: "sup-acct", Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.t0", SuppressedCount: 1, ExpiresAt: time.Now().Add(time.Hour)}))
	_, err := f.pool.Exec(f.ctx, `UPDATE purchase_executions SET status='approved' WHERE execution_id=$1`, root.ExecutionID)
	require.NoError(t, err, "a human approves after the tick read the row")

	err = f.m.failExistingPlanStep(f.ctx, root, "reason")
	require.ErrorIs(t, err, errExecutionNotNotifiable)
	var n int
	require.NoError(t, f.pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM purchase_suppressions WHERE execution_id = $1`, root.ExecutionID).Scan(&n))
	require.Equal(t, 1, n, "the approved row keeps its suppressions")
}
