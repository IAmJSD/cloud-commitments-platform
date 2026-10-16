//go:build integration

package config

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// managedTargetMetRefusal names what a refusal may carry: the
// unavailable-root sentinel, or the duplicate-root conflict its resolution is
// built on. An unrelated failure must not pass for a refusal.
func managedTargetMetRefusal(err error) bool {
	return errors.Is(err, ErrExecutionNotInExpectedStatus) || errors.Is(err, ErrPurchasePlanConflict)
}

func TestManagedTargetMetRefusesBeforeWrite(t *testing.T) {
	for _, which := range []string{"missing", "foreign", "finished", "wrong step", "occupied", "scheduled", "duplicate", "legacy foreign", "legacy other step", "beside root", "other step root"} {
		t.Run(which, func(t *testing.T) {
			ctx := t.Context()
			f := managedClockFixture(ctx, t, 1)
			ids := []string{f.planID}
			step, rootID := 2, ""
			if which != "missing" {
				rootID = f.row(ctx, t, 2, nil, "pending").ExecutionID
			}
			switch which {
			case "foreign", "legacy foreign":
				if which == "legacy foreign" {
					_, err := f.store.db.Exec(ctx, `UPDATE purchase_plans SET ramp_schedule=jsonb_set(ramp_schedule,'{dates_managed}','false') WHERE id=$1`, f.planID)
					require.NoError(t, err)
				}
				other, err := f.store.GetPurchasePlan(ctx, f.planID)
				require.NoError(t, err)
				other.ID, other.Name = uuid.NewString(), "foreign target-met fixture"
				other.RampSchedule.CurrentStep, other.RampSchedule.DatesManaged = 0, false
				require.NoError(t, f.store.CreatePurchasePlan(ctx, other))
				ids = append(ids, other.ID)
				rootID = uuid.NewString()
				require.NoError(t, f.store.SavePurchaseExecution(ctx, &PurchaseExecution{PlanID: other.ID, ExecutionID: rootID,
					Status: "pending", StepNumber: 2, ScheduledDate: other.RampSchedule.GetNextPurchaseDate()}))
				var plans, rows int
				require.NoError(t, f.store.db.QueryRow(ctx, `SELECT
					(SELECT count(*) FROM purchase_plans WHERE id::text=ANY($1::text[])),
					(SELECT count(*) FROM purchase_executions WHERE plan_id::text=ANY($1::text[]))`, ids).Scan(&plans, &rows))
				require.Equal(t, 2, plans)
				require.Equal(t, 2, rows)
			case "legacy other step":
				_, err := f.store.db.Exec(ctx, `UPDATE purchase_plans SET ramp_schedule=jsonb_set(ramp_schedule,'{dates_managed}','false') WHERE id=$1`, f.planID)
				require.NoError(t, err)
				rootID = f.row(ctx, t, 3, nil, "pending").ExecutionID
			case "beside root":
				rootID = ""
			case "other step root":
				step, rootID = 3, f.row(ctx, t, 3, nil, "pending").ExecutionID
			case "finished":
				_, err := f.store.db.Exec(ctx, `UPDATE purchase_plans SET ramp_schedule=jsonb_set(ramp_schedule,'{total_steps}','1') WHERE id=$1`, f.planID)
				require.NoError(t, err)
			case "wrong step":
				step = 3
			case "occupied":
				require.NoError(t, f.store.SavePurchaseExecution(ctx, &PurchaseExecution{PlanID: f.planID, ExecutionID: rootID,
					Status: "pending", StepNumber: 2, Recommendations: []RecommendationRecord{baselineRec(uuid.NewString(), "m5.large", 1)}}))
			case "scheduled":
				_, err := f.store.db.Exec(ctx, `UPDATE purchase_executions SET status='scheduled',scheduled_execution_at=NOW()+INTERVAL '48 days' WHERE execution_id=$1`, rootID)
				require.NoError(t, err)
			case "duplicate":
				f.row(ctx, t, 2, nil, "pending")
			}
			query := `SELECT jsonb_build_object('plans',
				(SELECT jsonb_agg(to_jsonb(p) ORDER BY p.id) FROM purchase_plans p WHERE p.id::text=ANY($1::text[])),
				'executions',(SELECT jsonb_agg(to_jsonb(e) ORDER BY e.execution_id) FROM purchase_executions e WHERE e.plan_id::text=ANY($1::text[])))`
			snapshot := func(row pgx.Row) []byte {
				var data []byte
				require.NoError(t, row.Scan(&data))
				return data
			}
			before := snapshot(f.store.db.QueryRow(ctx, query, ids))
			err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
				refused := f.store.CompleteTargetMetStepTx(ctx, tx, f.planID, step, rootID, "fixture target met")
				require.Equal(t, before, snapshot(tx.QueryRow(ctx, query, ids)), "managed target-met must refuse before any write")
				require.True(t, managedTargetMetRefusal(refused), "managed target-met must refuse an unavailable root, got %v", refused)
				return refused
			})
			require.Error(t, err)
			require.Equal(t, before, snapshot(f.store.db.QueryRow(ctx, query, ids)))
			require.Equal(t, 1, f.currentStep(ctx, t), "a refused managed target-met must leave the ramp where it was")
		})
	}
}
