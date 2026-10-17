//go:build integration
// +build integration

package config

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestDeletePurchasePlan_CancelsOpenStepRows covers issue #862: plan_id is ON
// DELETE SET NULL, and token approval applies the plan's step cap only while
// plan_id is set, so a step row still open when its plan is deleted must not
// survive as an approvable, plan-less row.
func TestDeletePurchasePlan_CancelsOpenStepRows(t *testing.T) {
	ctx := context.Background()
	f := newRampFixture(ctx, t, 1, 0, 4)
	acct := &f.accounts[0]

	open := map[string]*PurchaseExecution{}
	for _, status := range []string{"pending", "notified", "paused"} {
		open[status] = f.row(ctx, t, 1, acct, status)
	}
	settled := map[string]*PurchaseExecution{}
	for _, status := range []string{"scheduled", "approved", StatusCompleted, StatusFailed} {
		settled[status] = f.row(ctx, t, 1, acct, status)
	}
	suppress := func(execID string) {
		require.NoError(t, f.store.CreateSuppression(ctx, &PurchaseSuppression{
			ExecutionID: execID, AccountID: *acct, Provider: "aws", Service: "ec2",
			Region: "us-east-1", ResourceType: "m5.large", SuppressedCount: 1,
			ExpiresAt: time.Now().Add(24 * time.Hour),
		}))
	}
	suppress(open["pending"].ExecutionID)
	suppress(settled["scheduled"].ExecutionID)

	other := &PurchasePlan{Name: "other " + uuid.NewString()[:8], Services: map[string]ServiceConfig{}}
	require.NoError(t, f.store.CreatePurchasePlan(ctx, other))
	sibling := &PurchaseExecution{
		ExecutionID: uuid.NewString(), IdempotencyKey: uuid.NewString(), PlanID: other.ID,
		Status: "pending", StepNumber: 1, ScheduledDate: time.Now(),
	}
	require.NoError(t, f.store.SavePurchaseExecution(ctx, sibling))

	require.NoError(t, f.store.DeletePurchasePlan(ctx, f.planID))

	for status, exec := range open {
		got, err := f.store.GetExecutionByID(ctx, exec.ExecutionID)
		require.NoError(t, err)
		require.Equal(t, StatusCanceled, got.Status, "%s row of the deleted plan stays approvable", status)
		require.Empty(t, got.PlanID, "%s row is detached from the deleted plan", status)
		_, err = f.store.TransitionExecutionStatus(ctx, exec.ExecutionID, []string{"pending", "notified", "paused"}, "running", nil)
		require.ErrorIs(t, err, ErrExecutionNotInExpectedStatus, "%s row can still be claimed for approval", status)
	}
	for status, exec := range settled {
		got, err := f.store.GetExecutionByID(ctx, exec.ExecutionID)
		require.NoError(t, err)
		require.Equal(t, status, got.Status, "%s row is not an open step row and keeps its status", status)
	}
	got, err := f.store.GetExecutionByID(ctx, sibling.ExecutionID)
	require.NoError(t, err)
	require.Equal(t, "pending", got.Status, "another plan's row is untouched")

	var pendingSup, scheduledSup int
	require.NoError(t, f.store.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM purchase_suppressions WHERE execution_id = $1`, open["pending"].ExecutionID).Scan(&pendingSup))
	require.NoError(t, f.store.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM purchase_suppressions WHERE execution_id = $1`, settled["scheduled"].ExecutionID).Scan(&scheduledSup))
	require.Zero(t, pendingSup, "the canceled row's suppression is released")
	require.Equal(t, 1, scheduledSup, "a row that was not canceled keeps its suppression")
}
