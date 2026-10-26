//go:build integration

package purchase

// Real PostgreSQL plus the ramp-step fixture's fake provider (labeled fixture;
// nothing here reaches a cloud API). Issue #532: a run that ends
// partially_completed bought part of its commitments, so the approve funnel
// mints it a revocation token as it does for a completed run.

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A fan-out root whose first account buys and whose other two fail on
// credentials ends partially_completed: the funnel returns the error, the row
// and a token whose hash is stored with the revocation window as its expiry.
func TestApproveAndExecute_PartialRunMintsRevocationToken(t *testing.T) {
	ctx := context.Background()
	f := newRampStepFixture(ctx, t)
	allowFailedNotification(f.manager.email.(*MockEmailSender))
	root := f.saveRootExecution(ctx, t, 3)
	before := time.Now()

	final, token, err := f.manager.ApproveAndExecute(ctx, root.ExecutionID, "approver@example.invalid", nil, nil)
	require.Error(t, err)
	require.NotNil(t, final)
	assert.True(t, IsCommittedPartialRun(final, err))
	require.NotEmpty(t, token, "a partial run is revocable, so it needs a revocation token")
	assert.Equal(t, 1, f.purchaseCount())

	row := rowStatus(ctx, t, f, root.ExecutionID)
	assert.Equal(t, config.StatusPartiallyCompleted, row.Status)
	assert.True(t, config.ApprovalTokenMatches(row.ApprovalToken, token), "the stored hash must match the returned token")
	require.NotNil(t, row.ApprovalTokenExpiresAt)
	assert.WithinRange(t, *row.ApprovalTokenExpiresAt,
		before.Add(config.RevocationWindow).Add(-time.Second), time.Now().Add(config.RevocationWindow).Add(time.Second))
}

// Nothing bought: no revocation token, and the row keeps its approval token.
func TestApproveAndExecute_FailedRunMintsNoToken(t *testing.T) {
	ctx := context.Background()
	f := newRampStepFixture(ctx, t)
	allowFailedNotification(f.manager.email.(*MockEmailSender))
	broken := f.accounts[1].ID
	row := &config.PurchaseExecution{
		PlanID: f.plan.ID, ExecutionID: uuid.New().String(), Status: "notified", StepNumber: 3,
		ScheduledDate: time.Now(), CloudAccountID: &broken, ApprovalToken: config.HashApprovalToken("approval-tok"),
		Recommendations: []config.RecommendationRecord{withAccount(rampStepRecommendation()[0], broken)},
	}
	require.NoError(t, f.store.SavePurchaseExecution(ctx, row))

	final, token, err := f.manager.ApproveAndExecute(ctx, row.ExecutionID, "approver@example.invalid", nil, nil)
	require.Error(t, err)
	require.NotNil(t, final)
	assert.Equal(t, "failed", final.Status)
	assert.False(t, IsCommittedPartialRun(final, err))
	assert.Empty(t, token)
	assert.Zero(t, f.purchaseCount())
	assert.True(t, config.ApprovalTokenMatches(rowStatus(ctx, t, f, row.ExecutionID).ApprovalToken, "approval-tok"))
}

// Issue #532 slice 3: the token approve returns the funnel's row, so its
// caller can answer a failure after the claim with the row's status. A
// partial run returns the partially_completed row with its revocation token,
// a failed run the failed row, and a claim that was never won no row at all.
func TestApproveExecution_ReturnsTheFinalRow(t *testing.T) {
	ctx := context.Background()
	f := newRampStepFixture(ctx, t)
	allowFailedNotification(f.manager.email.(*MockEmailSender))

	t.Run("partial run", func(t *testing.T) {
		root := f.saveRootExecution(ctx, t, 3)
		root.ApprovalToken = config.HashApprovalToken("approval-tok")
		require.NoError(t, f.store.SavePurchaseExecution(ctx, root))

		final, token, err := f.manager.ApproveExecution(ctx, root.ExecutionID, "approval-tok", "approver@example.invalid", nil)
		require.Error(t, err)
		require.NotNil(t, final, "a run that started returns its row")
		assert.Equal(t, root.ExecutionID, final.ExecutionID)
		assert.Equal(t, config.StatusPartiallyCompleted, final.Status)
		assert.Equal(t, config.StatusPartiallyCompleted, rowStatus(ctx, t, f, root.ExecutionID).Status, "the returned status is the stored one")
		require.NotEmpty(t, token)
		assert.True(t, config.ApprovalTokenMatches(rowStatus(ctx, t, f, root.ExecutionID).ApprovalToken, token))
	})

	t.Run("failed run, then a second approve that loses the claim", func(t *testing.T) {
		broken := f.accounts[1].ID
		row := &config.PurchaseExecution{
			PlanID: f.plan.ID, ExecutionID: uuid.New().String(), Status: "notified", StepNumber: 3,
			ScheduledDate: time.Now(), CloudAccountID: &broken, ApprovalToken: config.HashApprovalToken("approval-tok"),
			Recommendations: []config.RecommendationRecord{withAccount(rampStepRecommendation()[0], broken)},
		}
		require.NoError(t, f.store.SavePurchaseExecution(ctx, row))
		bought := f.purchaseCount()

		final, token, err := f.manager.ApproveExecution(ctx, row.ExecutionID, "approval-tok", "approver@example.invalid", nil)
		require.Error(t, err)
		require.NotNil(t, final)
		assert.Equal(t, "failed", final.Status)
		assert.Equal(t, "failed", rowStatus(ctx, t, f, row.ExecutionID).Status)
		assert.Empty(t, token)

		// Nothing was bought, so the approval token is unchanged and still
		// validates; the claim CAS is what refuses the second approve.
		again, token, err := f.manager.ApproveExecution(ctx, row.ExecutionID, "approval-tok", "approver@example.invalid", nil)
		require.ErrorIs(t, err, config.ErrExecutionNotInExpectedStatus)
		assert.Nil(t, again, "a lost claim never started a run, so no row comes back")
		assert.Empty(t, token)
		assert.Equal(t, bought, f.purchaseCount(), "neither approve bought anything")
	})
}
