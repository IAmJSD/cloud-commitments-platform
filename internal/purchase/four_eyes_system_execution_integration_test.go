//go:build integration

package purchase

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #233: the real sweep's row passes four-eyes for an identified approver only.
func TestFourEyes_SweepMintedRowIsApprovableByIdentifiedApprover(t *testing.T) {
	ctx := context.Background()
	store, _ := rampStepStore(ctx, t)
	attachGlobal(ctx, t, store)
	plan := attachPlanInDB(ctx, t, store, "four-eyes-sweep", time.Now().Add(24*time.Hour))
	m := &Manager{config: store, email: newCountingSender(ctx),
		resolver: &fakeResolver{recs: []config.RecommendationRecord{attachRec("rec-a", 2)}}}
	require.True(t, m.sendPlanNotification(ctx, plan))

	rows := stepRowsInDB(ctx, t, store, plan.ID)
	require.Len(t, rows, 1)
	require.Equal(t, "notified", rows[0].Status)
	assert.True(t, rows[0].CreatedBySystem, "sweep-minted row must be marked system-created")
	assert.Nil(t, rows[0].CreatedByUserID)

	global, err := store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	global.RequireDifferentApprover = true
	require.NoError(t, store.SaveGlobalConfig(ctx, global))

	require.NoError(t, m.enforceFourEyesPolicy(ctx, rows[0].ExecutionID, "approver@example.com", nil))
	require.ErrorIs(t, m.enforceFourEyesPolicy(ctx, rows[0].ExecutionID, "", nil), ErrFourEyesDenied)
}

func TestFourEyes_SweepFailedRowIsNotSystemCreated(t *testing.T) {
	ctx := context.Background()
	store, _ := rampStepStore(ctx, t)
	plan := atomicStepPlan(ctx, t, store) // no max_step_commitment: the cap fails the step
	m := &Manager{config: store, resolver: &fakeResolver{recs: []config.RecommendationRecord{attachRec("rec-a", 1)}}}
	_, _, _, _ = m.getOrCreateExecution(ctx, plan)

	rows := stepRowsInDB(ctx, t, store, plan.ID)
	require.Len(t, rows, 1)
	require.Equal(t, "failed", rows[0].Status)
	assert.False(t, rows[0].CreatedBySystem)
}
