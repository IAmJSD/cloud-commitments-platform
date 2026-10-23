package config

import (
	"fmt"
	"sort"
	"strings"
)

// DerivePlanProviders extracts the distinct set of providers a plan targets
// by parsing the keys of plan.Services. Keys use the "provider/service" form
// buildServiceConfig writes, or the legacy "provider:service" form migration
// 000040 wrote (internal/api findServiceKey accepts both).
//
// A key that names no provider is skipped: that can only narrow the set, so
// the check stays at least as strict. What it must never do is leave the set
// empty, because callers read an empty set as "no restriction" and would
// accept an account of any provider (issue #316). So when the plan has
// services but none of their keys names a provider, this returns an error.
// A nil plan, or a plan with no services, yields an empty set.
func DerivePlanProviders(plan *PurchasePlan) ([]string, error) {
	if plan == nil {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(plan.Services))
	for k := range plan.Services {
		if i := strings.IndexAny(k, "/:"); i > 0 {
			seen[k[:i]] = struct{}{}
		}
	}
	if len(seen) == 0 && len(plan.Services) > 0 {
		return nil, fmt.Errorf("none of the plan's %d service keys names a provider", len(plan.Services))
	}
	providers := make([]string, 0, len(seen))
	for provider := range seen {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	return providers, nil
}
