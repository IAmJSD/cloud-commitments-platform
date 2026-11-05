package config

import (
	"fmt"
	"sort"
	"strings"
)

// DerivePlanProviders extracts the distinct set of providers a plan targets.
// Each service entry names its provider through its key, in the
// "provider/service" form buildServiceConfig writes or the legacy
// "provider:service" form migration 000040 wrote (internal/api findServiceKey
// accepts both). An entry whose key has neither form, such as a legacy alias
// kept under its original key, is identified by its own Provider field, the
// same field the legacy edit path (mergeRequestedServices) and plan step
// resolution go by.
//
// An entry identified neither way is skipped: that can only narrow the set,
// so the check stays at least as strict. What it must never do is leave the
// set empty, because callers read an empty set as "no restriction" and would
// accept an account of any provider (issue #316). So when the plan has
// services but none of them identifies a provider, this returns an error. A
// nil plan, or a plan with no services, yields an empty set.
func DerivePlanProviders(plan *PurchasePlan) ([]string, error) {
	if plan == nil {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(plan.Services))
	for k := range plan.Services {
		if provider := serviceEntryProvider(k, plan.Services[k].Provider); provider != "" {
			seen[provider] = struct{}{}
		}
	}
	if len(seen) == 0 && len(plan.Services) > 0 {
		return nil, fmt.Errorf("none of the plan's %d services identifies a provider by key or provider field", len(plan.Services))
	}
	providers := make([]string, 0, len(seen))
	for provider := range seen {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	return providers, nil
}

// serviceEntryProvider returns the provider a services entry names: the key's
// prefix when the key has one, else the entry's Provider field, else "".
func serviceEntryProvider(key, fieldProvider string) string {
	if i := strings.IndexAny(key, "/:"); i > 0 {
		return key[:i]
	}
	return strings.TrimSpace(fieldProvider)
}

// DerivePlanServices extracts the distinct service names a plan targets from
// the "provider/service" and "provider:service" keys of plan.Services, sorted.
func DerivePlanServices(plan *PurchasePlan) []string {
	if plan == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(plan.Services))
	for k := range plan.Services {
		if i := strings.IndexAny(k, "/:"); i > 0 && i < len(k)-1 {
			seen[k[i+1:]] = struct{}{}
		}
	}
	services := make([]string, 0, len(seen))
	for service := range seen {
		services = append(services, service)
	}
	sort.Strings(services)
	return services
}

// BoundKeySeparator joins a bound entry's "provider/service" key to its group
// id: "aws/ec2#<group_id>" (issue #1746).
const BoundKeySeparator = "#"

// IsBoundServiceKey reports whether a services key names a bound group entry.
func IsBoundServiceKey(key string) bool {
	return strings.Contains(key, BoundKeySeparator)
}
