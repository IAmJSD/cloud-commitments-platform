//go:build integration
// +build integration

package config

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The no-op row and the ramp advance commit together, after a baseline write
// that bumped updated_at in the same transaction; a failed zero-buy fan-out
// still cannot advance through the unchanged gate.
func TestCompleteTargetMetStepTx_RealPlan(t *testing.T) {
	ctx := context.Background()
	f := newRampFixture(ctx, t, 1, 1, 4)
	r := baselineRec(f.accounts[0], "m5.large", 100)
	f.rowWithRecs(ctx, t, 1, &f.accounts[0], StatusCompleted, false, r)
	failed := f.rowWithRecs(ctx, t, 1, &f.accounts[0], StatusFailed, false, r)
	failed.Recommendations = nil
	key := baselineKey(t, r)

	require.NoError(t, f.store.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := f.store.LockPurchasePlanTx(ctx, tx, f.planID)
		require.NoError(t, err)
		_, err = f.store.SetPlanRampBaselineTx(ctx, tx, f.planID, PlanRampBaseline{key: {Count: 100}})
		require.NoError(t, err)
		return f.store.CompleteTargetMetStepTx(ctx, tx, f.planID, 2, "", "target already met: target 40, committed 40")
	}))
	assert.Equal(t, 2, f.currentStep(ctx, t))
	assert.NotNil(t, f.baseline(ctx, t))
	var reason, note, status string
	require.NoError(t, f.store.db.QueryRow(ctx,
		`SELECT pre_approval_skip_reason, error, status FROM purchase_executions WHERE plan_id = $1 AND step_number = 2`, f.planID).Scan(&reason, &note, &status))
	assert.Equal(t, PreApprovalSkipRampTargetMet, reason)
	assert.Equal(t, StatusCompleted, status)
	assert.Contains(t, note, "1 failed row(s) of unknown outcome counted as committed")

	// A step whose only row failed without buying is still refused.
	f.rowWithRecs(ctx, t, 3, nil, StatusFailed, true)
	require.ErrorIs(t, f.store.CompletePlanStep(ctx, f.planID, 3), ErrRampStepIncomplete)
	assert.Equal(t, 2, f.currentStep(ctx, t))

	// A root row that is no longer pending/notified is not completed in place.
	done := f.rowWithRecs(ctx, t, 3, nil, StatusCompleted, false)
	err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
		return f.store.CompleteTargetMetStepTx(ctx, tx, f.planID, 3, done.ExecutionID, "n")
	})
	require.ErrorIs(t, err, ErrExecutionNotInExpectedStatus)
	assert.Equal(t, 2, f.currentStep(ctx, t))
}
