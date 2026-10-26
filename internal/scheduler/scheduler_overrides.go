package scheduler

import (
	"context"
	"fmt"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// applyAccountOverrides drops recs that the per-account override marks
// disabled, or whose engine / region / resource_type is excluded by the
// resolved (global ⊕ override) ServiceConfig.
//
// Architecturally this is the read-time half of issue #196 (Option B from
// issue #111). PR #193 already wired the override into the purchase forms;
// this helper applies the remaining override fields (enabled, include /
// exclude lists) to the recs list returned by Scheduler.ListRecommendations.
//
// Why post-DB rather than pushing into ListStoredRecommendations' WHERE clause:
//   - The override table is sparse (most accounts have no row).
//   - Filtering by include/exclude TEXT[] arrays per-(account, provider, service)
//     would force a correlated subquery per row — messier than the in-Go pass.
//   - The existing applySuppressions helper sets the precedent of post-DB
//     filtering inside ListRecommendations.
//
// Errors from the resolver are non-fatal: we log and pass the un-filtered
// list through, matching applySuppressions. Over-show is the safer default
// (an unconfigured-looking dashboard is recoverable; a blanked one is not).
func (s *Scheduler) applyAccountOverrides(ctx context.Context, recs []config.RecommendationRecord) ([]config.RecommendationRecord, error) {
	if len(recs) == 0 {
		return recs, nil
	}
	resolved, err := config.ResolveAccountConfigsForRecs(ctx, s.config, recs)
	if err != nil {
		return recs, fmt.Errorf("resolve account configs: %w", err)
	}
	if len(resolved) == 0 {
		return recs, nil
	}
	return filterRecsByResolvedConfigs(recs, resolved), nil
}

// filterRecsByResolvedConfigs drops recs whose resolved per-account
// ServiceConfig says enabled=false, whose engine / region / resource_type
// is rejected by the resolved include/exclude lists, or whose count is below
// the min_count floor (see overrideDimensions).
//
// Recs without a CloudAccountID use global configuration. Recs whose triple
// has no resolved entry pass through unfiltered.
func filterRecsByResolvedConfigs(
	recs []config.RecommendationRecord,
	resolved map[string]*config.ServiceConfig,
) []config.RecommendationRecord {
	// Allocate a fresh backing array so callers that hold a reference to
	// the original recs slice do not see mutations (05-M1).
	out := make([]config.RecommendationRecord, 0, len(recs))
	for i := range recs {
		rec := recs[i]
		accountID := ""
		if rec.CloudAccountID != nil {
			accountID = *rec.CloudAccountID
		}
		cfg := resolved[config.AccountConfigKey(accountID, rec.Provider, rec.Service)]
		if cfg == nil {
			out = append(out, rec)
			continue
		}
		if !passesOverrideDimensions(&rec, cfg) {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// overrideDimension is one axis of the account-service override filter:
// passes reports whether rec survives it under cfg, and reason is the
// hidden_by string the rec-detail view reports when it does not.
type overrideDimension struct {
	passes func(rec *config.RecommendationRecord, cfg *config.ServiceConfig) bool
	reason string
}

// overrideDimensions is the single list both the list filter
// (filterRecsByResolvedConfigs) and the rec-detail explainer
// (overrideHiddenReasons) walk, so a rec the list hides always carries a
// reason in hidden_by. Add a new filter dimension here, never to only one of
// the two callers: min_count was once added to the filter alone, and a
// min-count-filtered rec then opened with an empty hidden_by (#120).
var overrideDimensions = []overrideDimension{
	{reason: "enabled=false", passes: func(_ *config.RecommendationRecord, cfg *config.ServiceConfig) bool {
		return cfg.Enabled
	}},
	{reason: "engine", passes: func(rec *config.RecommendationRecord, cfg *config.ServiceConfig) bool {
		return engineMatches(rec.Engine, cfg)
	}},
	{reason: "region", passes: func(rec *config.RecommendationRecord, cfg *config.ServiceConfig) bool {
		return inListRule(rec.Region, cfg.IncludeRegions, cfg.ExcludeRegions)
	}},
	{reason: "resource_type", passes: func(rec *config.RecommendationRecord, cfg *config.ServiceConfig) bool {
		return inListRule(rec.ResourceType, cfg.IncludeTypes, cfg.ExcludeTypes)
	}},
	{reason: "min_count", passes: func(rec *config.RecommendationRecord, cfg *config.ServiceConfig) bool {
		return common.IsSavingsPlan(common.ServiceType(rec.Service)) || meetsMinCount(rec.Count, cfg)
	}},
}

// passesOverrideDimensions reports whether rec survives every override
// dimension under cfg, stopping at the first failure.
func passesOverrideDimensions(rec *config.RecommendationRecord, cfg *config.ServiceConfig) bool {
	for _, d := range overrideDimensions {
		if !d.passes(rec, cfg) {
			return false
		}
	}
	return true
}

// overrideHiddenReasons returns a non-empty slice when rec would be dropped by
// the given resolved ServiceConfig, naming each failing dimension in
// overrideDimensions order.
func overrideHiddenReasons(rec *config.RecommendationRecord, cfg *config.ServiceConfig) []string {
	var reasons []string
	for _, d := range overrideDimensions {
		if !d.passes(rec, cfg) {
			reasons = append(reasons, d.reason)
		}
	}
	return reasons
}

// meetsMinCount implements the read-time half of the GUI/CLI --min-count
// filter: a rec is dropped when its count is below cfg.MinCount. A MinCount
// of 0 (the default) disables the filter, matching the CLI flag's
// 0-no-floor semantics. count is the persisted RecommendationRecord.Count
// (already grace-suppression-adjusted by the time this runs, so the floor
// applies to the net count the user would actually see). Callers skip it
// for Savings Plans: their Count is always 1 (dollar-denominated), not an
// instance count.
func meetsMinCount(count int, cfg *config.ServiceConfig) bool {
	if cfg.MinCount <= 0 {
		return true
	}
	return count >= cfg.MinCount
}

// engineMatches applies the engine include/exclude rule with a "lax for
// engine-less services" carve-out.
//
// Some recommendation services (Savings Plans, Compute) carry no engine
// field. Strictly applying an IncludeEngines list would silently drop every
// such rec the moment a user added an engine include for a different service
// on the same (account, provider). The carve-out: when rec.Engine == "" we
// skip the engine check entirely and let the region / type filters decide.
func engineMatches(engine string, cfg *config.ServiceConfig) bool {
	if engine == "" {
		return true
	}
	return inListRule(engine, cfg.IncludeEngines, cfg.ExcludeEngines)
}

// inListRule returns true when value is allowed by an include/exclude pair:
//   - empty include list = allow anything not on the exclude list
//   - non-empty include list = value must be on it AND not on exclude
//
// Order matters: exclude wins over include when the same value appears on both.
func inListRule(value string, include, exclude []string) bool {
	for _, e := range exclude {
		if e == value {
			return false
		}
	}
	if len(include) == 0 {
		return true
	}
	for _, i := range include {
		if i == value {
			return true
		}
	}
	return false
}
