//go:build integration
// +build integration

package config

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntegration_ExecutionReaders_NullErrorColumn is the issue #1465
// regression. purchase_executions.error is nullable, and one row with a NULL
// there used to fail the scan for the whole result set, hiding every
// actionable execution from the history page and approval queue.
func TestIntegration_ExecutionReaders_NullErrorColumn(t *testing.T) {
	ctx := context.Background()
	store := setupRampStepStore(ctx, t)

	base := time.Now().Add(-time.Hour)
	saveClean := func(offset int, status string) string {
		exec := &PurchaseExecution{
			ExecutionID:    uuid.New().String(),
			IdempotencyKey: uuid.New().String(),
			Status:         status,
			ScheduledDate:  base.Add(time.Duration(offset) * time.Minute),
		}
		require.NoError(t, store.SavePurchaseExecution(ctx, exec))
		return exec.ExecutionID
	}
	other := saveClean(1, "pending")
	nullErr := saveClean(2, "pending")
	last := saveClean(3, "notified")
	// Migration 000146 (issue #1484) made the column NOT NULL. The scanner
	// still tolerates NULL for a schema rolled back below 000146, so relax
	// the constraint in this test's own database to seed the legacy shape.
	_, err := store.db.Exec(ctx, `ALTER TABLE purchase_executions ALTER COLUMN error DROP NOT NULL`)
	require.NoError(t, err)
	_, err = store.db.Exec(ctx, `UPDATE purchase_executions SET error = NULL WHERE execution_id = $1`, nullErr)
	require.NoError(t, err)

	wantIDs := []string{last, nullErr, other}
	assertAll := func(t *testing.T, got []PurchaseExecution) {
		t.Helper()
		ids := make([]string, len(got))
		for i := range got {
			ids[i] = got[i].ExecutionID
			assert.Empty(t, got[i].Error, "NULL error scans as empty string")
		}
		assert.Equal(t, wantIDs, ids)
	}

	statuses := []string{"pending", "notified"}
	byStatuses, err := store.GetExecutionsByStatuses(ctx, statuses, 100)
	require.NoError(t, err)
	assertAll(t, byStatuses)

	page, err := store.ListHistoryExecutionsPage(ctx, HistoryExecutionsPageQuery{Statuses: statuses, Limit: 100})
	require.NoError(t, err)
	assertAll(t, page)

	pending, err := store.GetPendingExecutions(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 3)
	for i := range pending {
		assert.Empty(t, pending[i].Error)
	}

	single, err := store.GetExecutionByID(ctx, nullErr)
	require.NoError(t, err)
	assert.Empty(t, single.Error)
}
