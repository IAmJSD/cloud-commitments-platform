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
