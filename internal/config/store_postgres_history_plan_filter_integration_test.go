//go:build integration

package config

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createHistoryTestPlan(ctx context.Context, t *testing.T, store *PostgresStore, name string) string {
	t.Helper()
	next := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	plan := &PurchasePlan{
		Name: name, Enabled: true,
		Services:          map[string]ServiceConfig{"ec2": {Provider: "aws", Service: "ec2", Enabled: true}},
		RampSchedule:      RampSchedule{Type: "weekly", PercentPerStep: 25, StepIntervalDays: 7, TotalSteps: 4, StartDate: time.Now().AddDate(0, 0, -7)},
		NextExecutionDate: &next,
	}
	require.NoError(t, plan.Validate())
	require.NoError(t, store.CreatePurchasePlan(ctx, plan))
	return plan.ID
}

// TestIntegration_HistoryPlanIDFilter pins the plan-history view's plan_id
// filter (issue #1404): the row listings and the summary aggregate return only
// that plan's purchases and executions, it narrows (never widens) the account
// filter, and no plan filter returns everything including planless rows.
func TestIntegration_HistoryPlanIDFilter(t *testing.T) {
	ctx := context.Background()
	f := newHistorySummaryFixture(ctx, t)
	planA := createHistoryTestPlan(ctx, t, f.store, "history-plan-a")
	planB := createHistoryTestPlan(ctx, t, f.store, "history-plan-b")
	a, b := f.accountA, f.accountB

	savePlanPurchase := func(day int, planID, provider, extID string, cloudAccountID *string) string {
		rec := &PurchaseHistoryRecord{
			AccountID: extID, PurchaseID: uuid.NewString(), Timestamp: f.base.Add(time.Duration(day) * 24 * time.Hour),
			Provider: provider, Service: "ec2", Region: "us-east-1", ResourceType: "t3.micro", Count: 1, Term: 1,
			Payment: "no-upfront", EstimatedSavings: 1, PlanID: planID, CloudAccountID: cloudAccountID,
		}
		require.NoError(t, f.store.SavePurchaseHistory(ctx, rec))
		return rec.PurchaseID
	}
	pa1 := savePlanPurchase(1, planA, "aws", "ext-sum-a", &a)
	pa2 := savePlanPurchase(2, planA, "azure", "ext-sum-b", &b)
	savePlanPurchase(3, planB, "aws", "ext-sum-a", &a)
	savePlanPurchase(4, "", "aws", "ext-sum-a", &a) // not tied to any plan

	saveExec := func(planID, status string) {
		exec := &PurchaseExecution{
			PlanID: planID, ExecutionID: uuid.NewString(), IdempotencyKey: uuid.NewString(), Status: status,
			Error: "boom", ScheduledDate: f.base.Add(time.Hour), CloudAccountID: &a,
		}
		require.NoError(t, f.store.SavePurchaseExecution(ctx, exec))
	}
	saveExec(planA, "failed")
	saveExec(planB, "failed")
	saveExec(planB, "failed")

	purchaseIDs := func(filter PurchaseHistoryFilter) []string {
		t.Helper()
		filter.Limit = MaxListLimit
		page, err := f.store.ListPurchaseHistoryPage(ctx, filter, nil)
		require.NoError(t, err)
		listed, err := f.store.GetPurchaseHistoryFiltered(ctx, filter)
		require.NoError(t, err)
		require.Len(t, listed, len(page), "the two listings share one predicate")
		ids := make([]string, 0, len(page))
		for i := range page {
			ids = append(ids, page[i].PurchaseID)
		}
		return ids
	}
	totals := func(filter PurchaseHistoryFilter) (purchases, executions int) {
		t.Helper()
		agg, err := f.store.HistorySummaryAggregate(ctx, HistorySummaryFilter{History: filter, ExecutionStatuses: []string{"failed"}})
		require.NoError(t, err)
		for i := range agg.Purchases {
			purchases += agg.Purchases[i].Count
		}
		for i := range agg.Executions {
			executions += agg.Executions[i].Count
		}
		return purchases, executions
	}

	assert.ElementsMatch(t, []string{pa1, pa2}, purchaseIDs(PurchaseHistoryFilter{PlanID: planA}))
	p, e := totals(PurchaseHistoryFilter{PlanID: planA})
	assert.Equal(t, 2, p)
	assert.Equal(t, 1, e)
	p, e = totals(PurchaseHistoryFilter{PlanID: planB})
	assert.Equal(t, 1, p)
	assert.Equal(t, 2, e)

	// The plan filter narrows the account filter instead of replacing it.
	assert.Equal(t, []string{pa1}, purchaseIDs(PurchaseHistoryFilter{PlanID: planA, AccountIDs: []string{a}}))
	p, _ = totals(PurchaseHistoryFilter{PlanID: planA, AccountIDs: []string{a}})
	assert.Equal(t, 1, p)

	// A plan with no rows matches nothing; no plan filter matches everything.
	assert.Empty(t, purchaseIDs(PurchaseHistoryFilter{PlanID: uuid.NewString()}))
	p, e = totals(PurchaseHistoryFilter{PlanID: uuid.NewString()})
	assert.Zero(t, p+e)
	assert.Len(t, purchaseIDs(PurchaseHistoryFilter{}), 4)
	p, e = totals(PurchaseHistoryFilter{})
	assert.Equal(t, 4, p)
	assert.Equal(t, 3, e)
}
