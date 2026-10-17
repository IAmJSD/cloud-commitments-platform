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
	f := newRampFixture(ctx, t, 1, 0, 4)
	r := baselineRec(f.accounts[0], "m5.large", 100)
	f.rowWithRecs(ctx, t, 1, &f.accounts[0], StatusCompleted, false, r)
	failed := f.rowWithRecs(ctx, t, 1, &f.accounts[0], StatusFailed, false, r)
	failed.Recommendations = nil
	key := baselineKey(t, r)

	require.NoError(t, f.store.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := f.store.LockPurchasePlanTx(ctx, tx, f.planID)
		require.NoError(t, err)
		wrote, err := f.store.SetPlanRampBaselineTx(ctx, tx, f.planID, PlanRampBaseline{key: {Count: 100}})
		require.NoError(t, err)
		require.True(t, wrote)
		_, err = tx.Exec(ctx, `UPDATE purchase_plans SET ramp_schedule=jsonb_set(ramp_schedule,'{current_step}','1') WHERE id=$1`, f.planID)
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

// G3: a zero-buy failed fan-out on the step refuses the target-met advance too.
func TestTargetMet_TargetMetRefusedOverFailedFanOut(t *testing.T) {
	ctx := context.Background()
	f := newRampFixture(ctx, t, 1, 1, 4)
	f.rowWithRecs(ctx, t, 2, &f.accounts[0], StatusFailed, true)
	err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := f.store.LockPurchasePlanTx(ctx, tx, f.planID); err != nil {
			return err
		}
		return f.store.CompleteTargetMetStepTx(ctx, tx, f.planID, 2, "", "target already met")
	})
	require.ErrorIs(t, err, ErrRampStepIncomplete)
	assert.Equal(t, 1, f.currentStep(ctx, t))
	var n int
	require.NoError(t, f.store.db.QueryRow(ctx, `SELECT count(*) FROM purchase_executions WHERE plan_id=$1 AND pre_approval_skip_reason=$2`, f.planID, PreApprovalSkipRampTargetMet).Scan(&n))
	assert.Equal(t, 0, n)
}

// A pending root that already carries attached recommendations is never completed as a no-op.
func TestTargetMet_RootWithRecsNotCompleted(t *testing.T) {
	ctx := context.Background()
	f := newRampFixture(ctx, t, 1, 1, 4)
	root := f.rowWithRecs(ctx, t, 2, nil, "pending", false, baselineRec(f.accounts[0], "m5.large", 5))
	err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
		return f.store.CompleteTargetMetStepTx(ctx, tx, f.planID, 2, root.ExecutionID, "n")
	})
	require.ErrorIs(t, err, ErrExecutionNotInExpectedStatus)
	assert.Equal(t, 1, f.currentStep(ctx, t))
	var st string
	require.NoError(t, f.store.db.QueryRow(ctx, `SELECT status FROM purchase_executions WHERE execution_id=$1`, root.ExecutionID).Scan(&st))
	assert.Equal(t, "pending", st)
}

// The unknown-outcome count excludes failed rows that never submitted.
func TestTargetMet_UnknownCountExcludesNoSubmission(t *testing.T) {
	ctx := context.Background()
	f := newRampFixture(ctx, t, 1, 1, 4)
	r := baselineRec(f.accounts[0], "m5.large", 3)
	f.rowWithRecs(ctx, t, 1, &f.accounts[0], StatusCompleted, false, r)
	f.rowWithRecs(ctx, t, 1, &f.accounts[0], StatusFailed, true, r)
	require.NoError(t, f.store.WithTx(ctx, func(tx pgx.Tx) error {
		return f.store.CompleteTargetMetStepTx(ctx, tx, f.planID, 2, "", "met")
	}))
	var note string
	require.NoError(t, f.store.db.QueryRow(ctx, `SELECT error FROM purchase_executions WHERE plan_id=$1 AND pre_approval_skip_reason=$2`, f.planID, PreApprovalSkipRampTargetMet).Scan(&note))
	assert.Contains(t, note, "; 0 failed row(s) of unknown outcome")
}

// A failed, submitted row whose recommendations are JSON null (scalar) must not break the count.
func TestTargetMet_UnknownCountScalarRecs(t *testing.T) {
	ctx := context.Background()
	f := newRampFixture(ctx, t, 1, 1, 4)
	r := baselineRec(f.accounts[0], "m5.large", 3)
	f.rowWithRecs(ctx, t, 1, &f.accounts[0], StatusCompleted, false, r)
	f.rowWithRecs(ctx, t, 1, &f.accounts[0], StatusFailed, false)
	var typ string
	require.NoError(t, f.store.db.QueryRow(ctx, `SELECT coalesce(jsonb_typeof(recommendations),'sqlnull') FROM purchase_executions WHERE plan_id=$1 AND status=$2`, f.planID, StatusFailed).Scan(&typ))
	t.Logf("recommendations jsonb type of the failed row: %s", typ)
	require.NoError(t, f.store.WithTx(ctx, func(tx pgx.Tx) error {
		return f.store.CompleteTargetMetStepTx(ctx, tx, f.planID, 2, "", "met")
	}))
}

// A pre-created empty root (recommendations JSON null) completes in place.
func TestTargetMet_NullRootCompletes(t *testing.T) {
	ctx := context.Background()
	f := newRampFixture(ctx, t, 1, 1, 4)
	f.rowWithRecs(ctx, t, 1, &f.accounts[0], StatusCompleted, false, baselineRec(f.accounts[0], "m5.large", 3))
	root := f.rowWithRecs(ctx, t, 2, nil, "pending", false)
	require.NoError(t, f.store.WithTx(ctx, func(tx pgx.Tx) error {
		return f.store.CompleteTargetMetStepTx(ctx, tx, f.planID, 2, root.ExecutionID, "met")
	}))
	assert.Equal(t, 2, f.currentStep(ctx, t))
}
