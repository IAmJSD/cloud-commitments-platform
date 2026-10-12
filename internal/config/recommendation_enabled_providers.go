package config

// FilterRecommendationsByEnabledProviders drops recs whose Provider is absent
// from enabled. An empty enabled list is the permissive default (nothing is
// configured), so recs pass through unchanged. Shared by Opportunities and
// the plan-step resolver so both show and buy the same set.
func FilterRecommendationsByEnabledProviders(recs []RecommendationRecord, enabled []string) []RecommendationRecord {
	if len(enabled) == 0 {
		return recs
	}
	enabledSet := make(map[string]struct{}, len(enabled))
	for _, p := range enabled {
		enabledSet[p] = struct{}{}
	}
	filtered := make([]RecommendationRecord, 0, len(recs))
	for i := range recs {
		if _, ok := enabledSet[recs[i].Provider]; ok {
			filtered = append(filtered, recs[i])
		}
	}
	return filtered
}
