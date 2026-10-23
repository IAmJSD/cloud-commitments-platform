package api

import (
	"encoding/json"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/require"
)

func b1s1Service(provider, service string, enabled bool) config.ServiceConfig {
	return config.ServiceConfig{Provider: provider, Service: service, Enabled: enabled, Term: 1, Payment: "monthly", Coverage: 40,
		RampSchedule: "monthly-10pct", MinCount: 7,
		IncludeEngines: []string{"postgres"}, ExcludeEngines: []string{"mysql"},
		IncludeRegions: []string{"west"}, ExcludeRegions: []string{"east"},
		IncludeTypes: []string{"large"}, ExcludeTypes: []string{"small"}}
}

func TestLegacyServiceB1S1Aliases(t *testing.T) {
	for _, alias := range []struct{ provider, old, canonical string }{
		{"azure", "vm", "compute"}, {"azure", "sql", "relational-db"}, {"azure", "redis", "cache"},
		{"gcp", "cloudsql", "relational-db"}, {"gcp", "memorystore", "cache"}, {"azure", "compute", "compute"},
	} {
		for _, enabled := range []bool{false, true} {
			t.Run(alias.provider+"/"+alias.old+"/"+map[bool]string{true: "enabled", false: "disabled"}[enabled], func(t *testing.T) {
				stored := b1s1Service(alias.provider, alias.old, enabled)
				existing := map[string]config.ServiceConfig{"original-unusual-key": stored}
				before, err := json.Marshal(existing)
				require.NoError(t, err)
				req := config.ServiceConfig{Provider: alias.provider, Service: alias.canonical, Enabled: true, Term: 3, Payment: "upfront", Coverage: 91}
				requested := map[string]config.ServiceConfig{alias.provider + "/" + alias.canonical: req}
				requestBefore, err := json.Marshal(requested)
				require.NoError(t, err)
				merged, err := mergeRequestedServices(existing, requested)
				require.NoError(t, err)
				want := stored
				want.Provider, want.Service, want.Term, want.Payment, want.Coverage = req.Provider, req.Service, req.Term, req.Payment, req.Coverage
				require.Equal(t, map[string]config.ServiceConfig{"original-unusual-key": want}, merged)
				after, err := json.Marshal(existing)
				require.NoError(t, err)
				require.Equal(t, before, after, "caller-owned map and all backing fields remain unchanged")
				after, err = json.Marshal(requested)
				require.NoError(t, err)
				require.Equal(t, requestBefore, after)
			})
		}
	}
}

func TestLegacyServiceB1S1ExistingMergeControls(t *testing.T) {
	req := config.ServiceConfig{Provider: "aws", Service: "savings-plans-compute", Enabled: true, Term: 3, Payment: "no-upfront", Coverage: 90}
	requested := map[string]config.ServiceConfig{"aws/savings-plans-compute": req}
	for _, keys := range [][]string{{"aws/savings-plans-compute", "aws:savings-plans-compute"}, {"aws:savings-plans-compute"}} {
		existing := map[string]config.ServiceConfig{}
		for _, key := range keys {
			existing[key] = b1s1Service("aws", "savings-plans", false)
		}
		before, err := json.Marshal(existing)
		require.NoError(t, err)
		merged, err := mergeRequestedServices(existing, requested)
		require.NoError(t, err)
		want := overlayServiceConfig(existing[keys[0]], req)
		require.Equal(t, want, merged[keys[0]], "key precedence wins over inconsistent stored AWS value")
		require.True(t, merged[keys[0]].Enabled, "exact-key Enabled overlay is unchanged")
		if len(keys) == 2 {
			require.Equal(t, existing[keys[1]], merged[keys[1]])
		}
		after, err := json.Marshal(existing)
		require.NoError(t, err)
		require.Equal(t, before, after)
	}
	for _, existing := range []map[string]config.ServiceConfig{nil, {}, {"old": b1s1Service("azure", "vm", false)}} {
		merged, err := mergeRequestedServices(existing, nil)
		require.NoError(t, err)
		require.Equal(t, existing, merged)
		merged, err = mergeRequestedServices(existing, map[string]config.ServiceConfig{})
		require.NoError(t, err)
		require.Equal(t, existing, merged)
	}
	for _, existing := range []map[string]config.ServiceConfig{nil, {}, {"plain": {Provider: "gcp", Service: "compute", Enabled: true}}} {
		merged, err := mergeRequestedServices(existing, requested)
		require.NoError(t, err)
		require.Equal(t, requested, merged, "empty map and genuinely different singleton retain replacement behavior")
	}
	_, err := mergeRequestedServices(map[string]config.ServiceConfig{"one": {}, "two": {}}, requested)
	ce, ok := IsClientError(err)
	require.True(t, ok)
	require.Equal(t, 400, ce.code)
}

func TestLegacyServiceB1S1IndependentReplacementControls(t *testing.T) {
	for _, tc := range []struct{ name, provider, service string }{
		{"same provider different canonical service", "azure", "cache"},
		{"different provider same canonical service", "gcp", "compute"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			existing := map[string]config.ServiceConfig{"original-key": b1s1Service("azure", "vm", false)}
			req := config.ServiceConfig{Provider: tc.provider, Service: tc.service, Enabled: true, Term: 3, Payment: "monthly", Coverage: 91}
			require.NoError(t, req.Validate())
			key := tc.provider + "/" + tc.service
			requested := map[string]config.ServiceConfig{key: req}
			before, err := json.Marshal(existing)
			require.NoError(t, err)
			requestBefore, err := json.Marshal(requested)
			require.NoError(t, err)
			merged, err := mergeRequestedServices(existing, requested)
			require.NoError(t, err)
			require.Equal(t, map[string]config.ServiceConfig{key: req}, merged, "replacement retains exactly requested key, Enabled and fields without hidden inheritance")
			after, err := json.Marshal(existing)
			require.NoError(t, err)
			require.Equal(t, before, after, "stored input and its backing fields remain unchanged")
			after, err = json.Marshal(requested)
			require.NoError(t, err)
			require.Equal(t, requestBefore, after, "requested input remains unchanged")
		})
	}
}
