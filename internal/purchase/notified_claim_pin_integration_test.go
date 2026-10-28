//go:build integration

package purchase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Real PostgreSQL, mock email sender and mock provider factory (labeled
// fixtures; nothing here reaches a cloud API).

func claimPinFixture(ctx context.Context, t *testing.T, store *config.PostgresStore) *config.PurchasePlan {
	t.Helper()
	global, err := store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	recipient := "fixture@example.invalid"
	global.NotificationEmail = &recipient
	global.AutoPurchaseEnabled = true
	require.NoError(t, store.SaveGlobalConfig(ctx, global))
	plan := atomicStepPlan(ctx, t, store)
	plan.MaxStepCommitment = f64(1000)
	require.NoError(t, store.UpdatePurchasePlan(ctx, plan))
	return plan
}

func claimPinRow(ctx context.Context, t *testing.T, store *config.PostgresStore, plan *config.PurchasePlan, status string, recs []config.RecommendationRecord) *config.PurchaseExecution {
	t.Helper()
	row := &config.PurchaseExecution{
		PlanID: plan.ID, ExecutionID: uuid.New().String(), Status: status, StepNumber: 1,
		ScheduledDate: time.Now().Add(-time.Hour), Recommendations: recs,
	}
	require.NoError(t, store.SavePurchaseExecution(ctx, row))
	return row
}

func statusInDB(ctx context.Context, t *testing.T, store *config.PostgresStore, id string) *config.PurchaseExecution {
	t.Helper()
	row, err := store.GetExecutionByID(ctx, id)
	require.NoError(t, err)
	return row
}

// S1: a failed email leaves the row pending and the scheduler refuses it; a
// sent email makes it notified, which the scheduler gate then accepts.
func TestNotifiedTransition_OnlyAfterEmailSucceeds(t *testing.T) {
	ctx := context.Background()
	store, _ := rampStepStore(ctx, t)
	plan := claimPinFixture(ctx, t, store)
	// The notification window is open for the plan's next date (tomorrow).
	row := claimPinRow(ctx, t, store, plan, "pending", []config.RecommendationRecord{upfrontRec(10)})
	factory := new(MockProviderFactory)

	sender := new(MockEmailSender)
	assertMocksOnCleanup(t, sender)
	sender.On("SendScheduledPurchaseNotification", ctx, mock.Anything).Return(errors.New("ses down")).Once()
	manager := &Manager{config: store, email: sender, providerFactory: factory}
	require.False(t, manager.sendPlanNotification(ctx, plan))
	assert.Equal(t, "pending", statusInDB(ctx, t, store, row.ExecutionID).Status)

	result, err := manager.ProcessScheduledPurchases(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, result.Processed, "a pending rec-bearing row is refused by the scheduler")
	assert.Equal(t, "pending", statusInDB(ctx, t, store, row.ExecutionID).Status)

	sender.On("SendScheduledPurchaseNotification", ctx, mock.Anything).Return(nil).Once()
	require.True(t, manager.sendPlanNotification(ctx, plan))
	notified := statusInDB(ctx, t, store, row.ExecutionID)
	assert.Equal(t, "notified", notified.Status)
	assert.NotEmpty(t, notified.ApprovalToken)

	// Notified now passes the scheduler gate (the claim itself is not run here).
	eligible, err := manager.executableByScheduler(ctx, notified)
	require.NoError(t, err)
	assert.True(t, eligible)
	factory.AssertNotCalled(t, "CreateAndValidateProvider", mock.Anything, mock.Anything, mock.Anything)
}

// attachingStore performs a recommendation attach between the gate and the
// claim, the race S2 closes.
type attachingStore struct {
	*config.PostgresStore
	onClaim func()
}

func (a *attachingStore) TransitionExecutionStatusPinned(ctx context.Context, id string, from []string, to string, actor *string, sawEmpty bool) (*config.PurchaseExecution, error) {
	a.onClaim()
	return a.PostgresStore.TransitionExecutionStatusPinned(ctx, id, from, to, actor, sawEmpty)
}

// S2 on real PostgreSQL: the gate sees a rec-less notified row, an attach lands,
// the claim must refuse (row untouched, still notified, recommendations kept)
// and nothing is bought. The next tick gates the attached row with the cap and
// the notified rule.
func TestClaimPin_RealAttachBetweenGateAndClaim(t *testing.T) {
	ctx := context.Background()
	store, _ := rampStepStore(ctx, t)
	plan := claimPinFixture(ctx, t, store)
	row := claimPinRow(ctx, t, store, plan, "notified", nil)
	factory := new(MockProviderFactory)
	attached := false
	wrapped := &attachingStore{PostgresStore: store, onClaim: func() {
		attached = true
		require.NoError(t, store.WithTx(ctx, func(tx pgx.Tx) error {
			ok, err := store.SetExecutionRecommendationsIfEmptyTx(ctx, tx, row.ExecutionID, []config.RecommendationRecord{upfrontRec(10)}, 10, 1)
			require.True(t, ok)
			return err
		}))
	}}
	manager := &Manager{config: wrapped, email: new(MockEmailSender), providerFactory: factory}

	result, err := manager.ProcessScheduledPurchases(ctx)
	require.NoError(t, err)
	require.True(t, attached, "the gate must have passed the rec-less row")
	assert.Equal(t, 0, result.Executed)
	assert.Equal(t, 0, result.Failed)
	after := statusInDB(ctx, t, store, row.ExecutionID)
	assert.Equal(t, "notified", after.Status, "the lost pin leaves the row alone")
	assert.Len(t, after.Recommendations, 1)
	factory.AssertNotCalled(t, "CreateAndValidateProvider", mock.Anything, mock.Anything, mock.Anything)
}
