//go:build integration
// +build integration

package config

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

const settleActor = "00000000-0000-0000-0000-0000000000a9"

// managedSettleFixture is a managed 4-step ramp plan with one account.
func managedSettleFixture(ctx context.Context, t *testing.T) *rampFixture {
	t.Helper()
	f := newRampFixture(ctx, t, 1, 1, 4)
	_, err := f.store.db.Exec(ctx, `UPDATE purchase_plans SET ramp_schedule = ramp_schedule || '{"dates_managed":true}' WHERE id = $1`, f.planID)
	require.NoError(t, err)
	return f
}

func (f *rampFixture) settle(ctx context.Context, execID, reason string) (*ExecutionSettlement, error) {
	return f.store.SettleFailedExecution(ctx, ExecutionSettleRequest{ExecutionID: execID, Actor: settleActor, Reason: reason, Acknowledge: true})
}

func (f *rampFixture) settled(ctx context.Context, t *testing.T, execID string) bool {
	t.Helper()
	var settled bool
	require.NoError(t, f.store.db.QueryRow(ctx, `SELECT outcome_settled_at IS NOT NULL FROM purchase_executions WHERE execution_id = $1`, execID).Scan(&settled))
	return settled
}

// A failed row with an unknown outcome blocks the managed delete until an
// operator settles it; then the delete detaches it without changing it, and the
// ledger still counts it as possibly bought (issue #999).
func TestSettleFailedExecutionLetsManagedDeleteProceed(t *testing.T) {
	ctx := context.Background()
	f := managedSettleFixture(ctx, t)
	rec := baselineRec(f.accounts[0], "m5.large", 4)
	failed := f.rowWithRecs(ctx, t, 1, &f.accounts[0], StatusFailed, false, rec)
	retry := f.retryInOneTx(ctx, t, failed, uuid.NewString(), StatusCompleted)
	retry.Recommendations = []RecommendationRecord{rec}
	require.NoError(t, f.store.SavePurchaseExecution(ctx, retry))
	ledger := f.committed(ctx, t)

	require.ErrorIs(t, f.store.DeletePurchasePlan(ctx, f.planID), ErrManagedPlanLiveExecutions)

	res, err := f.settle(ctx, failed.ExecutionID, "checked the account: retry bought it")
	require.NoError(t, err)
	require.False(t, res.Replayed)
	require.Equal(t, settleActor, res.SettledBy)
	require.Contains(t, res.Settlement, "chain_tail="+retry.ExecutionID+" tail_status=completed")
	require.Equal(t, ledger, f.committed(ctx, t), "settling must not change what the ledger counts")

	require.NoError(t, f.store.DeletePurchasePlan(ctx, f.planID))
	got, err := f.store.GetExecutionByID(ctx, failed.ExecutionID)
	require.NoError(t, err)
	require.Equal(t, StatusFailed, got.Status, "the settled row keeps its failed status")
	require.False(t, got.NoSubmission, "settling is not a no_submission proof")
	require.Empty(t, got.PlanID)
	require.True(t, f.settled(ctx, t, failed.ExecutionID), "the audit record survives the delete")
}

func TestSettleFailedExecutionRefusals(t *testing.T) {
	ctx := context.Background()
	f := managedSettleFixture(ctx, t)
	acct := &f.accounts[0]
	conflict := map[string]string{
		"completed":            f.row(ctx, t, 1, acct, StatusCompleted).ExecutionID,
		"pending":              f.row(ctx, t, 1, acct, "pending").ExecutionID,
		"canceled":             f.row(ctx, t, 1, acct, StatusCanceled).ExecutionID,
		"failed no_submission": f.rowWithRecs(ctx, t, 1, acct, StatusFailed, true).ExecutionID,
	}
	for _, tail := range []string{"pending", "running", StatusFailed} {
		head := f.row(ctx, t, 2, acct, StatusFailed)
		f.retryInOneTx(ctx, t, head, uuid.NewString(), tail)
		conflict["retry tail "+tail] = head.ExecutionID
	}
	adHoc := &PurchaseExecution{ExecutionID: uuid.NewString(), Status: StatusFailed, StepNumber: 1}
	require.NoError(t, f.store.SavePurchaseExecution(ctx, adHoc))
	conflict["no plan"] = adHoc.ExecutionID

	for name, id := range conflict {
		_, err := f.settle(ctx, id, "operator checked the account")
		require.ErrorIs(t, err, ErrExecutionSettleConflict, name)
		require.False(t, f.settled(ctx, t, id), name)
	}
	_, err := f.settle(ctx, uuid.NewString(), "operator checked the account")
	require.ErrorIs(t, err, ErrExecutionSettleNotFound)

	failed := f.row(ctx, t, 3, acct, StatusFailed)
	for name, req := range map[string]ExecutionSettleRequest{
		"no ack":    {ExecutionID: failed.ExecutionID, Actor: settleActor, Reason: "checked"},
		"no actor":  {ExecutionID: failed.ExecutionID, Reason: "checked", Acknowledge: true},
		"no reason": {ExecutionID: failed.ExecutionID, Actor: settleActor, Acknowledge: true},
	} {
		_, err := f.store.SettleFailedExecution(ctx, req)
		require.ErrorIs(t, err, ErrExecutionSettleInvalid, name)
	}
	require.False(t, f.settled(ctx, t, failed.ExecutionID))
}

// A failed tail blocks its predecessor's settle only while it can still be
// retried: once the tail is settled, the head can be too.
func TestSettleFailedExecutionChainOfUnknownOutcomes(t *testing.T) {
	ctx := context.Background()
	f := managedSettleFixture(ctx, t)
	head := f.row(ctx, t, 1, &f.accounts[0], StatusFailed)
	tail := f.retryInOneTx(ctx, t, head, uuid.NewString(), StatusFailed)

	_, err := f.settle(ctx, head.ExecutionID, "operator checked the account")
	require.ErrorIs(t, err, ErrExecutionSettleConflict)
	_, err = f.settle(ctx, tail.ExecutionID, "operator checked the account")
	require.NoError(t, err)
	require.ErrorIs(t, f.store.DeletePurchasePlan(ctx, f.planID), ErrManagedPlanLiveExecutions, "the unsettled head still blocks")
	_, err = f.settle(ctx, head.ExecutionID, "operator checked the account")
	require.NoError(t, err)
	require.NoError(t, f.store.DeletePurchasePlan(ctx, f.planID))
}

func TestSettleFailedExecutionReplayAndRetry(t *testing.T) {
	ctx := context.Background()
	f := managedSettleFixture(ctx, t)
	failed := f.row(ctx, t, 1, &f.accounts[0], StatusFailed)
	first, err := f.settle(ctx, failed.ExecutionID, "operator checked the account")
	require.NoError(t, err)

	again, err := f.settle(ctx, failed.ExecutionID, "operator checked the account")
	require.NoError(t, err)
	require.True(t, again.Replayed)
	require.True(t, first.SettledAt.Equal(again.SettledAt), "a replay writes nothing")
	_, err = f.settle(ctx, failed.ExecutionID, "a different reason entirely")
	require.ErrorIs(t, err, ErrExecutionSettleConflict)

	successor := &PurchaseExecution{ExecutionID: uuid.NewString(), PlanID: f.planID, Status: "pending", StepNumber: 1}
	require.NoError(t, f.store.WithTx(ctx, func(tx pgx.Tx) error {
		require.NoError(t, f.store.SavePurchaseExecutionTx(ctx, tx, successor))
		linked, linkErr := f.store.LinkRetryExecutionAtomic(ctx, tx, failed.ExecutionID, successor.ExecutionID)
		require.NoError(t, linkErr)
		require.False(t, linked, "a settled row must not be retried")
		return nil
	}))
}

// A retry that links the row first wins: the settle waits on the row lock,
// then sees the live successor and refuses.
func TestSettleWaitsForRetryAndRefusesItsLiveSuccessor(t *testing.T) {
	ctx := context.Background()
	f := managedSettleFixture(ctx, t)
	failed := f.row(ctx, t, 1, &f.accounts[0], StatusFailed)
	retryTx, err := f.store.db.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = retryTx.Rollback(ctx) })
	successor := &PurchaseExecution{ExecutionID: uuid.NewString(), PlanID: f.planID, Status: "pending", StepNumber: 1}
	require.NoError(t, f.store.SavePurchaseExecutionTx(ctx, retryTx, successor))
	linked, err := f.store.LinkRetryExecutionAtomic(ctx, retryTx, failed.ExecutionID, successor.ExecutionID)
	require.NoError(t, err)
	require.True(t, linked)

	done := make(chan error, 1)
	go func() {
		_, settleErr := f.settle(ctx, failed.ExecutionID, "operator checked the account")
		done <- settleErr
	}()
	testhelpers.WaitForLockWait(ctx, t, f.store.db, "%FOR UPDATE%")
	require.NoError(t, retryTx.Commit(ctx))
	require.ErrorIs(t, testhelpers.Await(t, done), ErrExecutionSettleConflict)
	require.False(t, f.settled(ctx, t, failed.ExecutionID))
}

// A delete that holds the plan lock first wins: the settle waits, then finds
// the row detached and refuses instead of settling it against no plan.
func TestSettleWaitsForDeleteAndRefusesDetachedRow(t *testing.T) {
	ctx := context.Background()
	f := newRampFixture(ctx, t, 1, 1, 4)
	failed := f.row(ctx, t, 1, &f.accounts[0], StatusFailed)
	del := testhelpers.HoldCommandFootprint(ctx, t, f.store.db, f.planID)

	done := make(chan error, 1)
	go func() {
		_, settleErr := f.settle(ctx, failed.ExecutionID, "operator checked the account")
		done <- settleErr
	}()
	testhelpers.WaitForLockWait(ctx, t, f.store.db, "%FOR SHARE%")
	_, err := del.Exec(ctx, `DELETE FROM purchase_plans WHERE id = $1`, f.planID)
	require.NoError(t, err)
	require.NoError(t, del.Commit(ctx))
	require.ErrorIs(t, testhelpers.Await(t, done), ErrExecutionSettleConflict)
	require.False(t, f.settled(ctx, t, failed.ExecutionID))
}

// A delete that queues behind a settle sees the committed settlement.
func TestDeleteWaitsForSettleAndSeesIt(t *testing.T) {
	ctx := context.Background()
	f := managedSettleFixture(ctx, t)
	failed := f.row(ctx, t, 1, &f.accounts[0], StatusFailed)
	settleTx, err := f.store.db.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = settleTx.Rollback(ctx) })
	// The settle's own lock footprint and write, paused before commit.
	_, err = settleTx.Exec(ctx, `SELECT 1 FROM purchase_plans WHERE id = $1 FOR SHARE`, f.planID)
	require.NoError(t, err)
	_, err = settleTx.Exec(ctx, `UPDATE purchase_executions SET outcome_settled_at = now(),
		outcome_settled_by = $2, outcome_settlement = 'operator:settled_unknown_outcome' WHERE execution_id = $1`, failed.ExecutionID, settleActor)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- f.store.DeletePurchasePlan(ctx, f.planID) }()
	testhelpers.WaitForLockWait(ctx, t, f.store.db, "%FOR UPDATE%")
	require.NoError(t, settleTx.Commit(ctx))
	require.NoError(t, testhelpers.Await(t, done))
}
