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

// first_dispatch_at is written only by the INSERT; a stale struct upserted
// later (nil or a different value) must not move or clear it.
func TestSavePurchaseExecution_FirstDispatchAtIsInsertOnly(t *testing.T) {
	ctx := context.Background()
	store := setupRampStepStore(ctx, t)

	stamp := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	exec := &PurchaseExecution{
		ExecutionID: uuid.New().String(), IdempotencyKey: uuid.New().String(),
		Status: "pending", StepNumber: 1, ScheduledDate: time.Now(), FirstDispatchAt: &stamp,
	}
	require.NoError(t, store.SavePurchaseExecution(ctx, exec))

	got, err := store.GetExecutionByID(ctx, exec.ExecutionID)
	require.NoError(t, err)
	require.NotNil(t, got.FirstDispatchAt, "insert persists the stamp")
	assert.True(t, stamp.Equal(*got.FirstDispatchAt))

	other := stamp.Add(30 * time.Minute)
	for _, stale := range []*time.Time{nil, &other} {
		exec.FirstDispatchAt = stale
		exec.Status = "running"
		require.NoError(t, store.SavePurchaseExecution(ctx, exec))
		got, err = store.GetExecutionByID(ctx, exec.ExecutionID)
		require.NoError(t, err)
		assert.Equal(t, "running", got.Status, "the upsert still updates status")
		require.NotNil(t, got.FirstDispatchAt, "upsert must not clear the stamp")
		assert.True(t, stamp.Equal(*got.FirstDispatchAt), "upsert must not move the stamp")
	}

	// An unstamped row stays unstamped when a later upsert carries a value.
	plain := &PurchaseExecution{ExecutionID: uuid.New().String(), IdempotencyKey: uuid.New().String(),
		Status: "pending", StepNumber: 1, ScheduledDate: time.Now()}
	require.NoError(t, store.SavePurchaseExecution(ctx, plain))
	plain.FirstDispatchAt = &other
	require.NoError(t, store.SavePurchaseExecution(ctx, plain))
	got, err = store.GetExecutionByID(ctx, plain.ExecutionID)
	require.NoError(t, err)
	assert.Nil(t, got.FirstDispatchAt, "upsert must not set the stamp")
}
