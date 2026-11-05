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

// ErrBoundPlanMissingBinding marks a bound plan entry with no bound
// recommendations, for example one stripped by an older binary's write
// (issue #1746). It wraps ErrUnsupportedPlan so attach records a failed step
// instead of buying by filter or retrying silently.
var ErrBoundPlanMissingBinding = fmt.Errorf("%w: bound plan entry has no bound recommendations", ErrUnsupportedPlan)

// Precheck refuses, with a plan-specific error, the plan settings Resolve does
// not apply: any Include*/Exclude*/MinCount filter on a service config (the
// plan-step request never sets them, so honoring them would buy something other
// than what the plan says), an armed multi-step plan with a Savings Plans
// service (platform#631 Q7), and an immediate ramp whose PercentPerStep is not
// 100 (an immediate ramp ignores that field when validated but Resolve sizes
// with it).
func Precheck(plan *config.PurchasePlan) error {
	if err := plan.RampSchedule.ValidateAllocationMode(); err != nil {
		return fmt.Errorf("%w: plan %s: %w", ErrUnsupportedPlan, plan.ID, err)
	}
	if err := CheckBoundBinding(plan); err != nil {
		return err
	}
	for _, k := range sortedServiceKeys(plan) {
		svc := plan.Services[k]
		if field := firstFilterSet(&svc, config.IsBoundServiceKey(k)); field != "" {
			return fmt.Errorf("%w: plan %s service %q sets %s", ErrUnsupportedPlan, plan.ID, k, field)
		}
	}
	if plan.RampSchedule.AllocationMode.SizesByPercent() && plan.RampSchedule.Type == "immediate" && plan.RampSchedule.PercentPerStep != 100 {
		return fmt.Errorf("%w: plan %s has an immediate ramp with percent_per_step %v (must be 100)",
			ErrUnsupportedPlan, plan.ID, plan.RampSchedule.PercentPerStep)
	}
	if err := plan.CheckNoMultiStepSavingsPlans(); err != nil {
		return fmt.Errorf("%w: plan %s: %w", ErrUnsupportedPlan, plan.ID, err)
	}
	return nil
}

// CheckBoundBinding refuses a plan where an enabled entry is bound (a "#" key,
// or any entry of a plan with the bound_groups marker) but has no bound
// identities. Precheck runs it, and so do plan writes that arm or widen
// auto-purchase; de-escalating writes skip it so a stripped plan can still be
// stopped.
func CheckBoundBinding(plan *config.PurchasePlan) error {
	marked := plan.RampSchedule.AllocationMode == config.RampAllocationBoundGroups
	for _, k := range sortedServiceKeys(plan) {
		svc := plan.Services[k]
		if svc.Enabled && (marked || config.IsBoundServiceKey(k)) && len(svc.BoundIdentities) == 0 {
			return fmt.Errorf("%w: plan %s service %q", ErrBoundPlanMissingBinding, plan.ID, k)
		}
	}
	return nil
}

func sortedServiceKeys(plan *config.PurchasePlan) []string {
	keys := make([]string, 0, len(plan.Services))
	for k := range plan.Services {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// firstFilterSet names the first filter field set on svc, or "". A bound
// entry's min_count of exactly 1 is its rollback sentinel (older binaries
// refuse any min_count), not a filter: every recommendation's count is >= 1.
func firstFilterSet(svc *config.ServiceConfig, bound bool) string {
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
	case svc.MinCount != 0 && (!bound || svc.MinCount != 1):
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
