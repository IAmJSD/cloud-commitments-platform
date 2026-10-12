package purchase

import (
	"errors"
	"fmt"
	"math"

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
