package config

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PreApprovalSkipRampTargetMet marks the completed no-op row a ramp step gets
// when its cumulative target is already held (platform#631). The advance gate
// (requireRampStepBought) is unchanged and still counts only completed or
// partially_completed rows as bought, so a failed zero-buy fan-out can never
// advance the ramp; only CompleteTargetMetStepTx writes this row, in the claim
// transaction, and the marker tells it apart from a purchase.
const PreApprovalSkipRampTargetMet = "ramp-target-met"

// CompleteTargetMetStepTx records a plan step whose cumulative target is
// already held as a completed no-op row and advances the ramp over it, all in
// the caller's transaction. Without a shared transaction a crash between the
// row and the advance would leave a settled step the ramp never moves past.
//
// rootID names the pre-created empty root row of the step to complete in place
// (compare-and-set: it must still be pending/notified and empty; otherwise
// ErrExecutionNotInExpectedStatus), or is "" to insert a new row. The note is
// stored as the row's error text; the number of failed rows of unknown outcome
// that were counted as committed is appended to it so an advance taken on such
// capacity is visible.
//
// The plan is re-read under the lock AFTER any baseline write in this
// transaction: that write bumps updated_at, and advancing from a plan read
// before it would lose the optimistic-lock check in UpdatePurchasePlanTx.
func (s *PostgresStore) CompleteTargetMetStepTx(ctx context.Context, tx pgx.Tx, planID string, stepNumber int, rootID, note string) error {
	if stepNumber <= 0 {
		return fmt.Errorf("cannot complete ramp step %d of plan %s: step numbers are 1-based", stepNumber, planID)
	}
	var unknown int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM purchase_executions
		 WHERE plan_id = $1 AND status = $2 AND NOT no_submission
		   AND CASE WHEN jsonb_typeof(recommendations) = 'array' THEN jsonb_array_length(recommendations) ELSE 0 END > 0`,
		planID, StatusFailed).Scan(&unknown); err != nil {
		return fmt.Errorf("failed to count unknown-outcome executions of plan %s: %w", planID, err)
	}
	note = fmt.Sprintf("%s; %d failed row(s) of unknown outcome counted as committed", note, unknown)
	now := time.Now()
	if rootID == "" {
		marker := PreApprovalSkipRampTargetMet
		exec := &PurchaseExecution{
			PlanID: planID, ExecutionID: uuid.New().String(), Status: StatusCompleted, StepNumber: stepNumber,
			ScheduledDate: now, CompletedAt: &now, Error: note,
			PreApprovalSkipReason: &marker,
		}
		if err := s.SavePurchaseExecutionTx(ctx, tx, exec); err != nil {
			return fmt.Errorf("failed to record target-met plan step %d of plan %s: %w", stepNumber, planID, err)
		}
	} else {
		tag, err := tx.Exec(ctx, `
			UPDATE purchase_executions
			   SET status = $2, completed_at = $3, error = $4, pre_approval_skip_reason = $5
			 WHERE execution_id = $1 AND status IN ('pending', 'notified')
			   AND CASE WHEN jsonb_typeof(recommendations) = 'array' THEN jsonb_array_length(recommendations) ELSE 0 END = 0`,
			rootID, StatusCompleted, now, note, PreApprovalSkipRampTargetMet)
		if err != nil {
			return fmt.Errorf("failed to complete target-met root row %s: %w", rootID, err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: root row %s", ErrExecutionNotInExpectedStatus, rootID)
		}
	}
	plan, err := s.LockPurchasePlanTx(ctx, tx, planID)
	if err != nil {
		return err
	}
	if plan == nil {
		return fmt.Errorf("%w: purchase plan %s", ErrNotFound, planID)
	}
	if err := advanceRampStep(ctx, tx, plan, stepNumber); err != nil {
		return err
	}
	return s.persistRampPosition(ctx, tx, plan)
}
