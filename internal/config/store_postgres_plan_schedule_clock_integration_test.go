//go:build integration

package config

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

func managedClockFixture(ctx context.Context, t *testing.T, current int) *rampFixture {
	t.Helper()
	f := newRampFixture(ctx, t, 0, current, 3)
	_, err := f.store.db.Exec(ctx, `UPDATE purchase_plans SET
		ramp_schedule = ramp_schedule || '{"dates_managed":true,"future_metadata":{"nested":9007199254740993}}',
		services = '{"future_service":{"enabled":false,"unknown_filter":["retained"]}}',
		ramp_baseline = '{"frozen":{"count":97}}' WHERE id = $1`, f.planID)
	require.NoError(t, err)
	return f
}

func nextClockRoot(ctx context.Context, t *testing.T, f *rampFixture, status string) *PurchaseExecution {
	t.Helper()
	root := f.row(ctx, t, 3, nil, status)
	date := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 47)
	_, err := f.store.db.Exec(ctx, `UPDATE purchase_executions SET scheduled_date = $2 WHERE execution_id = $1`, root.ExecutionID, date)
	require.NoError(t, err)
	root.ScheduledDate = date
	return root
}

func TestManagedCompletionKeepsNextRootAndRawPlan(t *testing.T) {
	for _, targetMet := range []bool{false, true} {
		t.Run(map[bool]string{false: "bought", true: "target met"}[targetMet], func(t *testing.T) {
			ctx := context.Background()
			f := managedClockFixture(ctx, t, 1)
			root := nextClockRoot(ctx, t, f, "pending")
			heldRec := baselineRec(uuid.NewString(), "m5.large", 17)
			f.rowWithRecs(ctx, t, 1, nil, StatusCompleted, false, heldRec)
			var before []byte
			heldBefore := f.committed(ctx, t)
			require.Equal(t, 17, heldBefore[baselineKey(t, heldRec)].Count)
			require.NoError(t, f.store.db.QueryRow(ctx, `SELECT jsonb_build_object('services',services,'baseline',ramp_baseline,
				'metadata',ramp_schedule->'future_metadata') FROM purchase_plans WHERE id=$1`, f.planID).Scan(&before))
			if targetMet {
				current := f.row(ctx, t, 2, nil, "pending")
				require.NoError(t, f.store.WithTx(ctx, func(tx pgx.Tx) error {
					return f.store.CompleteTargetMetStepTx(ctx, tx, f.planID, 2, current.ExecutionID, "target already held")
				}))
			} else {
				f.row(ctx, t, 2, nil, StatusCompleted)
				require.NoError(t, f.store.CompletePlanStep(ctx, f.planID, 2))
			}
			plan, err := f.store.GetPurchasePlan(ctx, f.planID)
			require.NoError(t, err)
			require.Equal(t, 2, plan.RampSchedule.CurrentStep)
			require.NotNil(t, plan.NextExecutionDate)
			require.True(t, root.ScheduledDate.Equal(*plan.NextExecutionDate), "completion must retain the exact next managed root date")
			var after []byte
			require.NoError(t, f.store.db.QueryRow(ctx, `SELECT jsonb_build_object('services',services,'baseline',ramp_baseline,
				'metadata',ramp_schedule->'future_metadata') FROM purchase_plans WHERE id=$1`, f.planID).Scan(&after))
			require.Equal(t, before, after)
			require.Equal(t, heldBefore, f.committed(ctx, t))
			var ramp map[string]json.RawMessage
			require.NoError(t, f.store.db.QueryRow(ctx, `SELECT ramp_schedule FROM purchase_plans WHERE id=$1`, f.planID).Scan(&after))
			require.NoError(t, json.Unmarshal(after, &ramp))
			require.JSONEq(t, "true", string(ramp["dates_managed"]))
		})
	}
}

func TestManagedCompletionStopsAtBlockedNextRoot(t *testing.T) {
	cases := []struct {
		name, status, sql string
		extra             string
		conflict, healthy bool
	}{
		{name: "missing"},
		{name: "canceled", status: StatusCanceled},
		{name: "failed", status: StatusFailed},
		{name: "paused", status: "paused"},
		{name: "legacy uncertain", status: "approved"},
		{name: "duplicate", status: "pending", extra: "duplicate", conflict: true},
		{name: "zero date", status: "pending", sql: "scheduled_date='0001-01-01T00:00:00Z'"},
		{name: "scheduled without delayed fire", status: "scheduled"},
		{name: "pending delayed fire", status: "pending", sql: "scheduled_execution_at=NOW()"},
		{name: "executed", status: "pending", sql: "executed_at=NOW()"},
		{name: "completed provenance", status: "pending", sql: "completed_at=NOW()"},
		{name: "skip provenance", status: "pending", sql: "pre_approval_skip_reason='unknown'"},
		{name: "purchased provenance", status: "pending", sql: `recommendations='[{"purchased":true}]'`},
		{name: "canceled purchased predecessor", status: "pending", extra: "canceled purchased"},
		{name: "canceled executed predecessor", status: "pending", extra: "canceled executed"},
		{name: "live child", status: "pending", extra: "child"},
		{name: "incoming other step", status: "pending", extra: "incoming step"},
		{name: "incoming other plan", status: "pending", extra: "incoming plan"},
		{name: "canceled unacted replacement", status: "pending", extra: "canceled", healthy: true},
		{name: "scheduled trustworthy", status: "scheduled", sql: "scheduled_execution_at=NOW()+INTERVAL '48 days'", healthy: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := managedClockFixture(ctx, t, 1)
			f.row(ctx, t, 2, nil, StatusCompleted)
			var root *PurchaseExecution
			if tc.status != "" {
				root = nextClockRoot(ctx, t, f, tc.status)
				if tc.sql != "" {
					_, err := f.store.db.Exec(ctx, "UPDATE purchase_executions SET "+tc.sql+" WHERE execution_id=$1", root.ExecutionID)
					require.NoError(t, err)
				}
			}
			managedClockSibling(ctx, t, f, root, tc.extra)
			err := f.store.CompletePlanStep(ctx, f.planID, 2)
			if tc.conflict {
				require.ErrorIs(t, err, ErrPurchasePlanConflict)
				require.Equal(t, 1, f.currentStep(ctx, t))
				return
			}
			require.NoError(t, err)
			plan, err := f.store.GetPurchasePlan(ctx, f.planID)
			require.NoError(t, err)
			require.Equal(t, 2, plan.RampSchedule.CurrentStep)
			if tc.healthy {
				require.NotNil(t, plan.NextExecutionDate)
				require.True(t, root.ScheduledDate.Equal(*plan.NextExecutionDate))
			} else {
				require.Nil(t, plan.NextExecutionDate, "blocked next root must stop the managed clock")
			}
		})
	}
}

func managedClockSibling(ctx context.Context, t *testing.T, f *rampFixture, root *PurchaseExecution, extra string) {
	t.Helper()
	if extra == "" {
		return
	}
	status := StatusCanceled
	if extra == "duplicate" || extra == "child" || extra == "incoming step" || extra == "incoming plan" {
		status = "pending"
	}
	owner, step := f, 3
	if extra == "incoming step" {
		step = 1
	}
	if extra == "incoming plan" {
		other, err := f.store.GetPurchasePlan(ctx, f.planID)
		require.NoError(t, err)
		other.ID, other.Name = "", "Other "+uuid.NewString()[:8]
		require.NoError(t, f.store.CreatePurchasePlan(ctx, other))
		owner = &rampFixture{store: f.store, planID: other.ID}
	}
	sibling := owner.row(ctx, t, step, nil, status)
	var err error
	switch extra {
	case "child":
		_, err = f.store.db.Exec(ctx, `UPDATE purchase_executions SET retry_attempt_n=1 WHERE execution_id=$1`, sibling.ExecutionID)
	case "canceled purchased":
		_, err = f.store.db.Exec(ctx, `UPDATE purchase_executions SET recommendations='[{"purchased":true}]' WHERE execution_id=$1`, sibling.ExecutionID)
	case "canceled executed":
		_, err = f.store.db.Exec(ctx, `UPDATE purchase_executions SET executed_at=NOW() WHERE execution_id=$1`, sibling.ExecutionID)
	case "incoming step", "incoming plan":
		_, err = f.store.db.Exec(ctx, `UPDATE purchase_executions SET retry_execution_id=$2 WHERE execution_id=$1`, sibling.ExecutionID, root.ExecutionID)
	}
	require.NoError(t, err)
}

func TestManagedCompletionFinishedAndLegacyClock(t *testing.T) {
	ctx := context.Background()
	f := managedClockFixture(ctx, t, 2)
	f.row(ctx, t, 3, nil, StatusCompleted)
	require.NoError(t, f.store.CompletePlanStep(ctx, f.planID, 3))
	plan, err := f.store.GetPurchasePlan(ctx, f.planID)
	require.NoError(t, err)
	require.Nil(t, plan.NextExecutionDate)
	legacy := newRampFixture(ctx, t, 0, 1, 3)
	legacy.row(ctx, t, 2, nil, StatusCompleted)
	require.NoError(t, legacy.store.CompletePlanStep(ctx, legacy.planID, 2))
	plan, err = legacy.store.GetPurchasePlan(ctx, legacy.planID)
	require.NoError(t, err)
	require.NotNil(t, plan.NextExecutionDate)
	require.WithinDuration(t, plan.RampSchedule.GetNextPurchaseDate(), *plan.NextExecutionDate, time.Millisecond) // Postgres stores microseconds
}
