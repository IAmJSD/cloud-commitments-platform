//go:build integration

package purchase

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Real PostgreSQL. Issue #73: the token/SQS cancel of a scheduled row used to
// reach CancelExecutionAtomic (status IN pending/notified), match zero rows and
// report a "concurrent operation" that never happened.

func cancelRow(ctx context.Context, t *testing.T, store *config.PostgresStore, planID, status string) *config.PurchaseExecution {
	t.Helper()
	row := &config.PurchaseExecution{
		PlanID: planID, ExecutionID: uuid.New().String(), Status: status, StepNumber: 1,
		ScheduledDate: time.Now().Add(time.Hour).Truncate(time.Microsecond),
		ApprovalToken: config.HashApprovalToken("tok"),
	}
	if status == "scheduled" {
		at := time.Now().Add(time.Hour).Truncate(time.Microsecond)
		row.ScheduledExecutionAt = &at
	}
	require.NoError(t, store.SavePurchaseExecution(ctx, row))
	return row
}

func TestCancelExecution_ScheduledRowIsRefusedWithRevokeHint(t *testing.T) {
	ctx := context.Background()
	store, _ := rampStepStore(ctx, t)
	plan := atomicStepPlan(ctx, t, store)
	manager := &Manager{config: store}

	t.Run("scheduled is refused with an accurate message", func(t *testing.T) {
		row := cancelRow(ctx, t, store, plan.ID, "scheduled")
		err := manager.CancelExecution(ctx, row.ExecutionID, "tok", "a@example.invalid")
		require.ErrorIs(t, err, config.ErrExecutionNotInExpectedStatus)
		assert.Contains(t, err.Error(), "current status: scheduled; use the revoke endpoint before it fires")
		assert.NotContains(t, err.Error(), "concurrent operation")
		assert.Equal(t, "scheduled", statusInDB(ctx, t, store, row.ExecutionID).Status)
	})

	t.Run("pending control still cancels", func(t *testing.T) {
		row := cancelRow(ctx, t, store, plan.ID, "pending")
		require.NoError(t, manager.CancelExecution(ctx, row.ExecutionID, "tok", "a@example.invalid"))
		got := statusInDB(ctx, t, store, row.ExecutionID)
		assert.Equal(t, config.StatusCanceled, got.Status)
		require.NotNil(t, got.CancelledBy)
		assert.Equal(t, "a@example.invalid", *got.CancelledBy)
	})
}
