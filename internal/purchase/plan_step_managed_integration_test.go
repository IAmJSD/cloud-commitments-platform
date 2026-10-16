//go:build integration

package purchase

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// Native store and Manager; recommendation resolver and email are recordings.
type managedClaimFixture struct {
	ctx      context.Context
	store    *config.PostgresStore
	pool     *pgxpool.Pool
	plan     *config.PurchasePlan
	m        *Manager
	resolver *fakeResolver
	sender   *countingSender
}

func newManagedClaimFixture(t *testing.T) *managedClaimFixture {
	t.Helper()
	ctx := t.Context()
	store, pool := rampStepStore(ctx, t)
	plan := attachPlanInDB(ctx, t, store, "managed-claim", time.Now().AddDate(0, 0, 47))
	resolver, sender := &fakeResolver{recs: []config.RecommendationRecord{attachRec("fixture", 1)}}, newCountingSender(ctx)
	return &managedClaimFixture{ctx, store, pool, plan, &Manager{config: store, email: sender, resolver: resolver}, resolver, sender}
}

func (f *managedClaimFixture) row(t *testing.T, step int, status string) *config.PurchaseExecution {
	t.Helper()
	e := &config.PurchaseExecution{PlanID: f.plan.ID, ExecutionID: uuid.NewString(), StepNumber: step, Status: status, ScheduledDate: *f.plan.NextExecutionDate, Recommendations: []config.RecommendationRecord{attachRec("fixture", 1)}}
	require.NoError(t, f.store.SavePurchaseExecution(f.ctx, e))
	return e
}

func (f *managedClaimFixture) seal(t *testing.T) {
	t.Helper()
	_, err := f.pool.Exec(f.ctx, `UPDATE purchase_plans SET ramp_schedule=ramp_schedule||'{"dates_managed":true}' WHERE id=$1`, f.plan.ID)
	require.NoError(t, err)
	f.plan, err = f.store.GetPurchasePlan(f.ctx, f.plan.ID)
	require.NoError(t, err)
}

func TestManagedClaimConservativeRoots(t *testing.T) {
	for _, tc := range []struct {
		name, status, sql, extra string
		adopt                    bool
	}{
		{name: "missing", extra: "later"},
		{name: "canceled", status: "canceled"}, {name: "failed", status: "failed"}, {name: "paused", status: "paused"}, {name: "uncertain", status: "approved"},
		{name: "duplicate", status: "pending", extra: "duplicate"},
		{name: "child", status: "pending", extra: "child"},
		{name: "retry", status: "pending", sql: "retry_attempt_n=1"},
		{name: "lineage", status: "pending", sql: "idempotency_key='fixture:child'"},
		{name: "outgoing", status: "pending", extra: "outgoing"},
		{name: "canceled acted", status: "pending", extra: "acted"},
		{name: "executed", status: "pending", sql: "executed_at=NOW()"},
		{name: "completed", status: "pending", sql: "completed_at=NOW()"},
		{name: "purchased", status: "pending", sql: "recommendations=jsonb_set(recommendations,'{0,purchased}','true')"},
		{name: "skip", status: "pending", sql: "pre_approval_skip_reason='prior'"},
		{name: "zero date", status: "pending", sql: "scheduled_date='0001-01-01T00:00:00Z'"},
		{name: "pending timer", status: "pending", sql: "scheduled_execution_at=NOW()"},
		{name: "scheduled missing timer", status: "scheduled"},
		{name: "scheduled", status: "scheduled", sql: "scheduled_execution_at=NOW()"},
		{name: "incoming step", status: "pending", extra: "incoming"},
		{name: "incoming foreign", status: "pending", extra: "foreign"},
		{name: "pending", status: "pending", adopt: true}, {name: "notified", status: "notified", adopt: true},
		{name: "replacement", status: "pending", extra: "unacted", adopt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newManagedClaimFixture(t)
			var root *config.PurchaseExecution
			if tc.status != "" {
				root = f.row(t, 1, tc.status)
			}
			if tc.sql != "" {
				_, err := f.pool.Exec(f.ctx, "UPDATE purchase_executions SET "+tc.sql+" WHERE execution_id=$1", root.ExecutionID)
				require.NoError(t, err)
			}
			switch tc.extra {
			case "later":
				f.row(t, 2, "pending")
			case "duplicate":
				f.row(t, 1, "pending")
			case "acted", "unacted":
				old := f.row(t, 1, "canceled")
				if tc.extra == "acted" {
					_, err := f.pool.Exec(f.ctx, `UPDATE purchase_executions SET completed_at=NOW() WHERE execution_id=$1`, old.ExecutionID)
					require.NoError(t, err)
				}
			case "child":
				var account string
				require.NoError(t, f.pool.QueryRow(f.ctx, `INSERT INTO cloud_accounts(name,provider,external_id) VALUES('managed-claim','aws','123456789012') RETURNING id`).Scan(&account))
				_, err := f.pool.Exec(f.ctx, `UPDATE purchase_executions SET cloud_account_id=$2 WHERE execution_id=$1`, root.ExecutionID, account)
				require.NoError(t, err)
			case "outgoing", "incoming", "foreign":
				other := f.row(t, 2, "failed")
				if tc.extra == "foreign" {
					p := attachPlanInDB(f.ctx, t, f.store, "foreign", *f.plan.NextExecutionDate)
					_, err := f.pool.Exec(f.ctx, `UPDATE purchase_executions SET plan_id=$2 WHERE execution_id=$1`, other.ExecutionID, p.ID)
					require.NoError(t, err)
				}
				from, to := other.ExecutionID, root.ExecutionID
				if tc.extra == "outgoing" {
					from, to = to, from
				}
				_, err := f.pool.Exec(f.ctx, `UPDATE purchase_executions SET retry_execution_id=$2 WHERE execution_id=$1`, from, to)
				require.NoError(t, err)
			}
			f.seal(t)
			before := stepRowsInDB(f.ctx, t, f.store, f.plan.ID)
			exec, token, _, err := f.m.getOrCreateExecution(f.ctx, f.plan)
			if tc.adopt {
				require.NoError(t, err)
				require.Equal(t, root.ExecutionID, exec.ExecutionID)
				require.NotEmpty(t, token)
				require.True(t, root.ScheduledDate.Equal(exec.ScheduledDate))
			} else {
				require.Error(t, err, "managed claim must stop")
				require.Nil(t, exec)
				require.Empty(t, token)
			}
			require.Equal(t, before, stepRowsInDB(f.ctx, t, f.store, f.plan.ID))
			require.Zero(t, f.resolver.calls.Load())
			require.Zero(t, f.sender.sends)
		})
	}
}

func TestManagedClaimEligibilityAndClock(t *testing.T) {
	for _, name := range []string{"disabled", "finished", "provider", "auto", "version", "step", "marker", "nil clock", "past clock", "empty", "inside veto"} {
		t.Run(name, func(t *testing.T) {
			f := newManagedClaimFixture(t)
			root := f.row(t, 1, "pending")
			if name == "disabled" || name == "finished" {
				_, err := f.pool.Exec(f.ctx, `UPDATE purchase_plans SET enabled=$2,ramp_schedule=jsonb_set(ramp_schedule,'{total_steps}',$3::jsonb) WHERE id=$1`, f.plan.ID, name != "disabled", map[bool]string{true: "0", false: "4"}[name == "finished"])
				require.NoError(t, err)
			}
			if name == "nil clock" || name == "past clock" {
				var clock *time.Time
				if name == "past clock" {
					date := time.Now().Add(-time.Hour)
					clock = &date
				}
				_, err := f.pool.Exec(f.ctx, `UPDATE purchase_plans SET next_execution_date=$2 WHERE id=$1`, f.plan.ID, clock)
				require.NoError(t, err)
			}
			if name == "empty" || name == "inside veto" {
				_, err := f.pool.Exec(f.ctx, `UPDATE purchase_executions SET recommendations='[]',scheduled_date=CASE WHEN $2 THEN NOW()+INTERVAL '1 day' ELSE scheduled_date END WHERE execution_id=$1`, root.ExecutionID, name == "inside veto")
				require.NoError(t, err)
			}
			f.seal(t)
			switch name {
			case "provider":
				svc := f.plan.Services["ec2"]
				svc.Provider = "azure"
				f.plan.Services["ec2"] = svc
			case "auto":
				f.plan.AutoPurchase = false
			case "version":
				f.plan.UpdatedAt = f.plan.UpdatedAt.Add(-time.Second)
			case "step":
				f.plan.RampSchedule.CurrentStep++
			case "marker":
				f.plan.RampSchedule.DatesManaged = false
			}
			before := stepRowsInDB(f.ctx, t, f.store, f.plan.ID)
			exec, token, _, err := f.m.getOrCreateExecution(f.ctx, f.plan)
			if name == "nil clock" || name == "past clock" || name == "empty" {
				require.NoError(t, err)
				require.Equal(t, root.ExecutionID, exec.ExecutionID)
				require.True(t, root.ScheduledDate.Equal(exec.ScheduledDate))
				require.NotEmpty(t, token)
			} else {
				require.Error(t, err)
				require.Nil(t, exec)
				require.Empty(t, token)
			}
			require.Len(t, stepRowsInDB(f.ctx, t, f.store, f.plan.ID), 1)
			if name == "inside veto" {
				require.Equal(t, before, stepRowsInDB(f.ctx, t, f.store, f.plan.ID))
			}
			require.EqualValues(t, map[bool]int{true: 1, false: 0}[name == "empty"], f.resolver.calls.Load())
		})
	}
}

func TestManagedManualDue(t *testing.T) {
	for _, state := range []string{"pending", "scheduled", "finished", "missing", "occupied"} {
		f := newManagedClaimFixture(t)
		root := f.row(t, 1, "pending")
		_, err := f.pool.Exec(f.ctx, `UPDATE purchase_executions SET recommendations=CASE WHEN $4 THEN recommendations ELSE '[]'::jsonb END,step_number=CASE WHEN $3 THEN 2 ELSE step_number END,scheduled_date=NOW()-INTERVAL '1 day',status=$2::text,scheduled_execution_at=CASE WHEN $2::text='scheduled' THEN NOW() END WHERE execution_id=$1`, root.ExecutionID, map[bool]string{true: "scheduled", false: "pending"}[state == "scheduled"], state == "missing", state == "occupied")
		require.NoError(t, err)
		_, err = f.pool.Exec(f.ctx, `UPDATE purchase_plans SET auto_purchase=false,ramp_schedule=jsonb_set(ramp_schedule,'{total_steps}',$2::jsonb) WHERE id=$1`, f.plan.ID, map[bool]string{true: "0", false: "4"}[state == "finished"])
		require.NoError(t, err)
		f.seal(t)
		var got *config.PurchaseExecution
		require.NoError(t, f.store.WithTx(f.ctx, func(tx pgx.Tx) error {
			var readErr error
			got, readErr = f.m.dueManualRootTx(f.ctx, tx, f.plan.ID)
			return readErr
		}))
		if state == "pending" {
			require.NotNil(t, got)
			require.Equal(t, root.ExecutionID, got.ExecutionID)
		} else {
			require.Nil(t, got)
		}
	}
}

func TestManagedClaimWaitsForOutcome(t *testing.T) {
	f := newManagedClaimFixture(t)
	root := f.row(t, 1, "pending")
	f.seal(t)
	ctx, cancel := context.WithTimeout(f.ctx, 20*time.Second)
	defer cancel()
	holder, err := f.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = holder.Rollback(context.Background()) }()
	_, err = holder.Exec(ctx, `SELECT execution_id FROM purchase_executions WHERE execution_id=$1 FOR UPDATE`, root.ExecutionID)
	require.NoError(t, err)
	type claimResult struct {
		token string
		err   error
	}
	done := make(chan claimResult, 1)
	go func() {
		_, token, _, claimErr := f.m.getOrCreateExecution(ctx, f.plan)
		done <- claimResult{token, claimErr}
	}()
	joined := false
	defer func() {
		cancel()
		if !joined {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("claim did not join")
			}
		}
	}()
	require.Eventually(t, func() bool {
		var n int
		readErr := f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT execution_id FROM purchase_executions%FOR UPDATE%'`).Scan(&n)
		return readErr == nil && n > 0
	}, 5*time.Second, 20*time.Millisecond)
	root.Status = "paused"
	require.NoError(t, f.store.SaveExecutionIfStatusTx(ctx, holder, root, []string{"pending"}))
	require.NoError(t, holder.Commit(ctx))
	select {
	case result := <-done:
		joined = true
		require.Error(t, result.err)
		require.Empty(t, result.token)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.Zero(t, f.resolver.calls.Load())
	rows := stepRowsInDB(f.ctx, t, f.store, f.plan.ID)
	require.Len(t, rows, 1)
	require.Equal(t, root.ExecutionID, rows[0].ExecutionID)
	require.Equal(t, "paused", rows[0].Status)
}
