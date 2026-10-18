//go:build integration
// +build integration

package config

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

func storedPlanJSON(ctx context.Context, t *testing.T, f *rampFixture) string {
	t.Helper()
	var stored string
	require.NoError(t, f.store.db.QueryRow(ctx, `SELECT to_jsonb(p)::text FROM purchase_plans p WHERE id = $1`, f.planID).Scan(&stored))
	return stored
}

func TestManagedPlanUpdateGuard(t *testing.T) {
	ctx := context.Background()
	f := managedClockFixture(ctx, t, 1)
	soon := time.Now().Add(48 * time.Hour)
	refused := map[string]func(p *PurchasePlan){
		"marker strip":        func(p *PurchasePlan) { p.RampSchedule.DatesManaged = false },
		"allocation mode":     func(p *PurchasePlan) { p.RampSchedule.AllocationMode = RampAllocationEqualCount },
		"ramp percent":        func(p *PurchasePlan) { p.RampSchedule.PercentPerStep = 50 },
		"current step":        func(p *PurchasePlan) { p.RampSchedule.CurrentStep = 2 },
		"service key":         func(p *PurchasePlan) { p.Services["ec2"] = ServiceConfig{Provider: "aws", Service: "ec2"} },
		"service content":     func(p *PurchasePlan) { p.Services["future_service"] = ServiceConfig{Term: 3} },
		"next date":           func(p *PurchasePlan) { p.NextExecutionDate = &soon },
		"notice":              func(p *PurchasePlan) { p.NotificationDaysBefore += 3 },
		"arming":              func(p *PurchasePlan) { p.AutoPurchase = true },
		"arming with rename":  func(p *PurchasePlan) { p.AutoPurchase, p.Name = true, "armed" },
		"notice with disable": func(p *PurchasePlan) { p.NotificationDaysBefore, p.Enabled = 9, false },
	}
	for name, mutate := range refused {
		t.Run("refuses "+name, func(t *testing.T) {
			plan, err := f.store.GetPurchasePlan(ctx, f.planID)
			require.NoError(t, err)
			before := storedPlanJSON(ctx, t, f)
			mutate(plan)
			require.ErrorIs(t, f.store.UpdatePurchasePlan(ctx, plan), ErrEditorReconciliationRequired)
			require.Equal(t, before, storedPlanJSON(ctx, t, f))
		})
	}

	t.Run("allows disarm, rename, cap and bookkeeping", func(t *testing.T) {
		_, err := f.store.db.Exec(ctx, `UPDATE purchase_plans SET auto_purchase = true WHERE id = $1`, f.planID)
		require.NoError(t, err)
		plan, err := f.store.GetPurchasePlan(ctx, f.planID)
		require.NoError(t, err)
		cap, now := 5.0, time.Now().Truncate(time.Microsecond)
		plan.Name, plan.Enabled, plan.AutoPurchase = "renamed", false, false
		plan.MaxStepCommitment, plan.LastExecutionDate, plan.LastNotificationSent = &cap, &now, &now
		require.NoError(t, f.store.UpdatePurchasePlan(ctx, plan))
		var name string
		var managed bool
		require.NoError(t, f.store.db.QueryRow(ctx, `SELECT name, ramp_schedule->>'dates_managed' = 'true' FROM purchase_plans WHERE id = $1`, f.planID).Scan(&name, &managed))
		require.Equal(t, "renamed", name)
		require.True(t, managed)
	})

	t.Run("allows a rebuilt identical plan", func(t *testing.T) {
		plan, err := f.store.GetPurchasePlan(ctx, f.planID)
		require.NoError(t, err)
		require.NoError(t, f.store.UpdatePurchasePlan(ctx, plan))
	})
}

func TestUnmanagedPlanUpdateGuard(t *testing.T) {
	ctx := context.Background()
	f := newRampFixture(ctx, t, 0, 1, 3)
	plan, err := f.store.GetPurchasePlan(ctx, f.planID)
	require.NoError(t, err)
	plan.Name, plan.NotificationDaysBefore, plan.AutoPurchase = "free", 7, true
	plan.RampSchedule.PercentPerStep = 40
	require.NoError(t, f.store.UpdatePurchasePlan(ctx, plan))

	t.Run("refuses marker adoption", func(t *testing.T) {
		plan.RampSchedule.DatesManaged = true
		before := storedPlanJSON(ctx, t, f)
		require.ErrorIs(t, f.store.UpdatePurchasePlan(ctx, plan), ErrEditorReconciliationRequired)
		require.Equal(t, before, storedPlanJSON(ctx, t, f))
	})

	t.Run("judges the locked row, not the caller's stale copy", func(t *testing.T) {
		stale, err := f.store.GetPurchasePlan(ctx, f.planID)
		require.NoError(t, err)
		_, err = f.store.db.Exec(ctx, `UPDATE purchase_plans SET ramp_schedule = ramp_schedule || '{"dates_managed":true}' WHERE id = $1`, f.planID)
		require.NoError(t, err)
		stale.Name = "stale rename"
		require.ErrorIs(t, f.store.UpdatePurchasePlan(ctx, stale), ErrEditorReconciliationRequired)
	})
}

func TestUpdatePurchasePlanTxGuardsDirectCallers(t *testing.T) {
	ctx := context.Background()
	f := managedClockFixture(ctx, t, 1)
	plan, err := f.store.GetPurchasePlan(ctx, f.planID)
	require.NoError(t, err)
	before := storedPlanJSON(ctx, t, f)
	plan.NotificationDaysBefore += 3
	err = f.store.WithTx(ctx, func(tx pgx.Tx) error { return f.store.UpdatePurchasePlanTx(ctx, tx, plan) })
	require.ErrorIs(t, err, ErrEditorReconciliationRequired)
	require.Equal(t, before, storedPlanJSON(ctx, t, f))
}
