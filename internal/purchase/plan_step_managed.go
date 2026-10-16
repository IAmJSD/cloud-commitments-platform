package purchase

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/jackc/pgx/v5"
)

type managedPlanRootStore interface {
	ResolveManagedPlanRootTx(context.Context, pgx.Tx, *config.PurchasePlan) (*config.PurchaseExecution, error)
}

func (m *Manager) managedPlanRootTx(ctx context.Context, tx pgx.Tx, plan *config.PurchasePlan) (*config.PurchaseExecution, error) {
	store, ok := m.config.(managedPlanRootStore)
	if !ok {
		return nil, fmt.Errorf("%w: managed root resolution unavailable", errPlanStepSkipped)
	}
	return store.ResolveManagedPlanRootTx(ctx, tx, plan)
}

func (m *Manager) claimManagedPlanStep(ctx context.Context, tx pgx.Tx, listed, locked *config.PurchasePlan, providers []string) (stepClaim, error) {
	if !managedPlanUnchanged(listed, locked, providers) {
		return stepClaim{}, errPlanStepSkipped
	}
	root, err := m.managedPlanRootTx(ctx, tx, locked)
	if err != nil {
		return stepClaim{}, err
	}
	if root == nil || root.Status == "scheduled" {
		return stepClaim{}, errPlanStepSkipped
	}
	locked.NextExecutionDate = &root.ScheduledDate
	return m.claimFromRows(ctx, tx, locked, stepClaim{number: locked.RampSchedule.CurrentStep + 1}, []config.PurchaseExecution{*root})
}

func managedPlanUnchanged(listed, locked *config.PurchasePlan, providers []string) bool {
	return locked.Enabled && locked.AutoPurchase == listed.AutoPurchase && locked.UpdatedAt.Equal(listed.UpdatedAt) &&
		locked.RampSchedule.CurrentStep == listed.RampSchedule.CurrentStep && strings.Join(planProviders(locked), ",") == strings.Join(providers, ",")
}

func enabledManualPlan(plan *config.PurchasePlan) bool {
	return plan != nil && plan.Enabled && !plan.AutoPurchase
}

func (m *Manager) dueManagedPlanRootTx(ctx context.Context, tx pgx.Tx, plan *config.PurchasePlan) (*config.PurchaseExecution, error) {
	root, err := m.managedPlanRootTx(ctx, tx, plan)
	if err != nil || root == nil {
		return nil, err
	}
	if root.Status != "scheduled" && len(root.Recommendations) == 0 && !root.ScheduledDate.After(time.Now()) {
		return root, nil
	}
	return nil, nil
}
