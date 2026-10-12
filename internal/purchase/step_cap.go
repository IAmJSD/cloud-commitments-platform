package purchase

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// ErrStepCommitmentCap marks a plan step refused by the per-plan spend cap.
var ErrStepCommitmentCap = errors.New("plan step refused by commitment cap")

// CheckStepCommitmentCap refuses a plan step's recommendations unless the plan
// carries a cap and the step's TOTAL commitment (upfront + monthly * term * 12,
// see RecommendationRecord.TotalCommitment) fits under it. It fails closed:
//   - a nil plan, a plan without MaxStepCommitment (nil or <= 0), or a
//     non-finite cap refuses everything;
//   - a recommendation with a zero, negative or non-finite total commitment is
//     refused (Azure monthly commitments have upfront 0, so upfront alone
//     would pass anything);
//   - no recommendations is not a step that can buy, so it is not this
//     check's concern and returns nil (the no-recommendations refusal owns it).
//
// It only ever refuses; nothing here buys.
func CheckStepCommitmentCap(plan *config.PurchasePlan, recs []config.RecommendationRecord) error {
	if len(recs) == 0 {
		return nil
	}
	if plan == nil || plan.MaxStepCommitment == nil || !(*plan.MaxStepCommitment > 0) || math.IsInf(*plan.MaxStepCommitment, 0) {
		return fmt.Errorf("%w: plan has no max_step_commitment set (auto-purchase of steps with recommendations is refused until one is set)", ErrStepCommitmentCap)
	}
	var total float64
	for i := range recs {
		c := recs[i].TotalCommitment()
		if !(c > 0) || math.IsInf(c, 0) {
			return fmt.Errorf("%w: recommendation %q has non-positive or non-finite total commitment %v", ErrStepCommitmentCap, recs[i].ID, c)
		}
		total += c
	}
	if total > *plan.MaxStepCommitment {
		return fmt.Errorf("%w: step total commitment %.2f exceeds max_step_commitment %.2f", ErrStepCommitmentCap, total, *plan.MaxStepCommitment)
	}
	return nil
}

// recheckStepCap re-reads the plan and re-applies CheckStepCommitmentCap to a
// row this worker has just claimed. A plan that cannot be read refuses too.
func (m *Manager) recheckStepCap(ctx context.Context, exec *config.PurchaseExecution) error {
	plan, err := m.config.GetPurchasePlan(ctx, exec.PlanID)
	if err != nil {
		return fmt.Errorf("failed to re-read plan %s for the post-claim cap check: %w", exec.PlanID, err)
	}
	return CheckStepCommitmentCap(plan, exec.Recommendations)
}

// failClaimedBeforeSubmission fails a claimed (running) row that was refused
// before any provider call, with the beforeSubmission class so its suppressions
// release. It returns the refusal, joined with ErrAuditLoss when the terminal
// save fails (the row would then be stranded in "running" for the reaper).
func (m *Manager) failClaimedBeforeSubmission(ctx context.Context, exec *config.PurchaseExecution, cause error) error {
	execErr := beforeSubmission(cause)
	m.finalizeExecution(exec, execErr)
	logging.Errorf("Refused claimed purchase %s before submission: %v", exec.ExecutionID, cause)
	if err := m.saveTerminalExecution(ctx, exec, execErr); err != nil {
		return fmt.Errorf("%w: terminal save failed (%w); original refusal: %w", config.ErrAuditLoss, err, execErr)
	}
	return execErr
}
