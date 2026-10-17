//go:build integration

package config

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

func TestRampRetention_NullServicesRemainEditable(t *testing.T) {
	ctx := context.Background()
	store := setupRampStepStore(ctx, t)
	p := &PurchasePlan{Name: "disabled fresh", RampSchedule: RampSchedule{Type: "weekly", PercentPerStep: 25, TotalSteps: 4, StepIntervalDays: 7}}
	require.NoError(t, store.CreatePurchasePlan(ctx, p))
	p.Name = "renamed"
	require.NoError(t, store.UpdatePurchasePlan(ctx, p))
	p.RampSchedule.StepIntervalDays = 30
	require.NoError(t, store.UpdatePurchasePlan(ctx, p))
	var state string
	require.NoError(t, store.db.QueryRow(ctx, `SELECT ramp_ledger_state FROM purchase_plans WHERE id=$1`, p.ID).Scan(&state))
	require.Equal(t, "new", state)
}

func TestRampRetention_FreshEditsRequireContinuousUnheldLineage(t *testing.T) {
	cases := []struct {
		name, status, lineage string
		noSubmission          bool
		count                 int
		want                  string
	}{
		{name: "fresh", want: "new"},
		{name: "empty pending", status: "pending", want: "legacy"},
		{name: "zero completed", status: StatusCompleted, want: "legacy"},
		{name: "selected completed", status: StatusCompleted, count: 20, want: "legacy"},
		{name: "unknown failed", status: StatusFailed, want: "legacy"},
		{name: "unsubmitted", status: StatusFailed, noSubmission: true, want: "new"},
		{name: "canceled", status: StatusCanceled, want: "new"},
		{name: "expired", status: StatusExpired, want: "new"},
		{name: "trusted", lineage: "trusted", want: "legacy"},
		{name: "legacy", lineage: "legacy", want: "legacy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newRampFixture(ctx, t, 0, 0, 4)
			if tc.status != "" {
				var recs []RecommendationRecord
				if tc.count > 0 {
					recs = []RecommendationRecord{{Selected: true, Count: tc.count}}
				}
				f.rowWithRecs(ctx, t, 1, nil, tc.status, tc.noSubmission, recs...)
			}
			if tc.lineage != "" {
				_, err := f.store.db.Exec(ctx, `UPDATE purchase_plans SET ramp_baseline='{"fixture":{"count":100}}', ramp_ledger_state=$2 WHERE id=$1`, f.planID, tc.lineage)
				require.NoError(t, err)
			}
			p, err := f.store.GetPurchasePlan(ctx, f.planID)
			require.NoError(t, err)
			p.RampSchedule.Type, p.RampSchedule.StepIntervalDays = "custom", 30
			require.NoError(t, f.store.UpdatePurchasePlan(ctx, p))
			var state string
			require.NoError(t, f.store.db.QueryRow(ctx, `SELECT ramp_ledger_state FROM purchase_plans WHERE id=$1`, f.planID).Scan(&state))
			require.Equal(t, tc.want, state)
		})
	}
}

func TestRampRetention_LegacyNeverRearmsAndFreezeIsAtomic(t *testing.T) {
	ctx := context.Background()
	f := newRampFixture(ctx, t, 0, 0, 4)
	require.NoError(t, f.store.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := f.store.LockPurchasePlanTx(ctx, tx, f.planID)
		if err != nil {
			return err
		}
		wrote, err := f.store.SetPlanRampBaselineTx(ctx, tx, f.planID, PlanRampBaseline{"fixture": {Count: 100}})
		require.True(t, wrote)
		return err
	}))
	_, err := f.store.db.Exec(ctx, `UPDATE purchase_plans SET ramp_baseline=NULL WHERE id=$1`, f.planID)
	require.NoError(t, err)
	_, err = f.store.db.Exec(ctx, `UPDATE purchase_plans SET ramp_ledger_state='new', ramp_schedule=jsonb_set(ramp_schedule,'{current_step}','0') WHERE id=$1`, f.planID)
	require.NoError(t, err)
	var state string
	require.NoError(t, f.store.db.QueryRow(ctx, `SELECT ramp_ledger_state FROM purchase_plans WHERE id=$1`, f.planID).Scan(&state))
	require.Equal(t, "legacy", state)
	err = f.store.WithTx(ctx, func(tx pgx.Tx) error {
		_, readErr := f.store.GetPlanRampBaselineTx(ctx, tx, f.planID)
		return readErr
	})
	require.ErrorIs(t, err, ErrRampLedgerUntrusted)
}
