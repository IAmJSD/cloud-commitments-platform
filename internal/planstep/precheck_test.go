package planstep

import (
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixtures are hand-built plans, not real configuration.

func planWith(svc config.ServiceConfig, ramp config.RampSchedule) *config.PurchasePlan {
	return &config.PurchasePlan{ID: "plan-pc", Services: map[string]config.ServiceConfig{"aws:ec2": svc}, RampSchedule: ramp}
}

func TestPrecheck(t *testing.T) {
	ok := config.ServiceConfig{Provider: "aws", Service: "ec2", Enabled: true}
	weekly := config.RampSchedule{Type: "weekly", PercentPerStep: 25}
	cases := []struct {
		name string
		plan *config.PurchasePlan
		want string // substring of the error; "" = accepted
	}{
		{"clean plan", planWith(ok, weekly), ""},
		{"immediate at 100", planWith(ok, config.RampSchedule{Type: "immediate", PercentPerStep: 100}), ""},
		{"immediate at 50", planWith(ok, config.RampSchedule{Type: "immediate", PercentPerStep: 50}), "immediate ramp"},
		{"immediate at 0", planWith(ok, config.RampSchedule{Type: "immediate"}), "immediate ramp"},
		{"weekly at 25 is not an immediate ramp", planWith(ok, weekly), ""},
		{"include engines", planWith(withSvc(ok, func(s *config.ServiceConfig) { s.IncludeEngines = []string{"mysql"} }), weekly), "include_engines"},
		{"exclude engines", planWith(withSvc(ok, func(s *config.ServiceConfig) { s.ExcludeEngines = []string{"mysql"} }), weekly), "exclude_engines"},
		{"include regions", planWith(withSvc(ok, func(s *config.ServiceConfig) { s.IncludeRegions = []string{"us-east-1"} }), weekly), "include_regions"},
		{"exclude regions", planWith(withSvc(ok, func(s *config.ServiceConfig) { s.ExcludeRegions = []string{"us-east-1"} }), weekly), "exclude_regions"},
		{"include types", planWith(withSvc(ok, func(s *config.ServiceConfig) { s.IncludeTypes = []string{"m5.large"} }), weekly), "include_types"},
		{"exclude types", planWith(withSvc(ok, func(s *config.ServiceConfig) { s.ExcludeTypes = []string{"m5.large"} }), weekly), "exclude_types"},
		{"min count", planWith(withSvc(ok, func(s *config.ServiceConfig) { s.MinCount = 1 }), weekly), "min_count"},
		{"negative min count", planWith(withSvc(ok, func(s *config.ServiceConfig) { s.MinCount = -1 }), weekly), "min_count"},
		{"filter on a disabled service still refuses", planWith(withSvc(ok, func(s *config.ServiceConfig) { s.Enabled = false; s.MinCount = 2 }), weekly), "min_count"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Precheck(tc.plan)
			if tc.want == "" {
				assert.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrUnsupportedPlan)
			assert.ErrorContains(t, err, tc.want)
			assert.ErrorContains(t, err, "plan-pc")
		})
	}
}

func withSvc(s config.ServiceConfig, f func(*config.ServiceConfig)) config.ServiceConfig {
	f(&s)
	return s
}

func TestCheckStoredSPCount(t *testing.T) {
	sp := func(count int) *config.RecommendationRecord {
		return &config.RecommendationRecord{ID: "r", Service: "savings-plans-compute", Count: count}
	}
	assert.NoError(t, checkStoredSPCount(sp(1)))
	for _, n := range []int{0, 2, -1} {
		require.ErrorIs(t, checkStoredSPCount(sp(n)), ErrUnsupportedPlan, "count %d", n)
	}
	// A count-denominated service is not constrained.
	assert.NoError(t, checkStoredSPCount(&config.RecommendationRecord{Service: "ec2", Count: 7}))
}

func TestResolveRefusesUnsupportedPlanBeforeReading(t *testing.T) {
	plan := azurePlan(100, 100)
	svc := plan.Services["azure:compute"]
	svc.MinCount = 3
	plan.Services["azure:compute"] = svc
	// A nil Source/Store would panic if Resolve read anything.
	_, err := Resolve(t.Context(), nil, nil, plan, accts)
	require.ErrorIs(t, err, ErrUnsupportedPlan)
}

func TestResolveRefusesStoredSPWithCountNotOne(t *testing.T) {
	stored := spRec(`{"hourly_commitment":1.234}`)
	stored.Count = 3
	_, err := Resolve(t.Context(), fakeSource{shown: []config.RecommendationRecord{stored}}, fakeStore{rows: []config.RecommendationRecord{stored}}, spPlan(80, 25), accts)
	require.ErrorIs(t, err, ErrUnsupportedPlan)
}
