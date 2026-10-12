//go:build integration

package purchase

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Real PostgreSQL, fixture rows (nothing attaches recommendations yet): the
// status lists the Plans-page pause and cancel handlers pass must match a
// notified, recommendation-bearing row in SQL, and a paused one must not be
// picked up by the scheduler.
func TestNotifiedRow_PauseCancelStatusListsMatchInSQL(t *testing.T) {
	ctx := context.Background()
	store, _ := rampStepStore(ctx, t)
	plan := claimPinFixture(ctx, t, store)
	recs := []config.RecommendationRecord{upfrontRec(10)}

	paused := claimPinRow(ctx, t, store, plan, "notified", recs)
	got, err := store.TransitionExecutionStatus(ctx, paused.ExecutionID, []string{"pending", "notified"}, "paused", nil)
	require.NoError(t, err)
	assert.Equal(t, "paused", got.Status)

	canceled := claimPinRow(ctx, t, store, plan, "notified", recs)
	got, err = store.TransitionExecutionStatus(ctx, canceled.ExecutionID, []string{"pending", "notified", "paused"}, "canceled", nil)
	require.NoError(t, err)
	assert.Equal(t, "canceled", got.Status)

	// Resume lands on pending, which the scheduler refuses for a rec-bearing row.
	got, err = store.TransitionExecutionStatus(ctx, paused.ExecutionID, []string{"paused"}, "pending", nil)
	require.NoError(t, err)
	manager := &Manager{config: store, providerFactory: new(MockProviderFactory)}
	result, err := manager.ProcessScheduledPurchases(ctx)
	require.NoError(t, err)
	assert.Zero(t, result.Processed)
	assert.Equal(t, "pending", statusInDB(ctx, t, store, got.ExecutionID).Status)
}
