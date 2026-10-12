package planstep

import (
	"errors"
	"fmt"
	"sort"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// ErrUnsupportedPlan marks a plan whose settings Resolve cannot honor. Every
// such plan is refused before any recommendation is read, rather than resolved
// with the setting silently ignored.
var ErrUnsupportedPlan = errors.New("plan settings are not supported by recommendation resolution")

// Precheck refuses, with a plan-specific error, the plan settings Resolve does
// not apply: any Include*/Exclude*/MinCount filter on a service config (the
// plan-step request never sets them, so honoring them would buy something other
// than what the plan says), and an immediate ramp whose PercentPerStep is not
// 100 (an immediate ramp ignores that field when validated but Resolve sizes
// with it).
func Precheck(plan *config.PurchasePlan) error {
	keys := make([]string, 0, len(plan.Services))
	for k := range plan.Services {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		svc := plan.Services[k]
		if field := firstFilterSet(&svc); field != "" {
			return fmt.Errorf("%w: plan %s service %q sets %s", ErrUnsupportedPlan, plan.ID, k, field)
		}
	}
	if plan.RampSchedule.Type == "immediate" && plan.RampSchedule.PercentPerStep != 100 {
		return fmt.Errorf("%w: plan %s has an immediate ramp with percent_per_step %v (must be 100)",
			ErrUnsupportedPlan, plan.ID, plan.RampSchedule.PercentPerStep)
	}
	return nil
}

// firstFilterSet names the first filter field set on svc, or "".
func firstFilterSet(svc *config.ServiceConfig) string {
	switch {
	case len(svc.IncludeEngines) > 0:
		return "include_engines"
	case len(svc.ExcludeEngines) > 0:
		return "exclude_engines"
	case len(svc.IncludeRegions) > 0:
		return "include_regions"
	case len(svc.ExcludeRegions) > 0:
		return "exclude_regions"
	case len(svc.IncludeTypes) > 0:
		return "include_types"
	case len(svc.ExcludeTypes) > 0:
		return "exclude_types"
	case svc.MinCount != 0:
		return "min_count"
	}
	return ""
}

// checkStoredSPCount refuses a Savings Plans stored row whose Count is not 1:
// an SP is sized by its hourly commitment, so any other count means the row is
// not the one-commitment shape the sizing assumes.
func checkStoredSPCount(stored *config.RecommendationRecord) error {
	if common.IsSavingsPlan(common.ServiceType(stored.Service)) && stored.Count != 1 {
		return fmt.Errorf("%w: savings plan stored row %s has count %d (must be 1)", ErrUnsupportedPlan, stored.ID, stored.Count)
	}
	return nil
}
