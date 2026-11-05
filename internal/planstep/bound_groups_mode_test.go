package planstep

import (
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixtures are hand-built plans, not real configuration (issue #1746).

func boundRamp(r config.RampSchedule) config.RampSchedule {
	r.AllocationMode = config.RampAllocationBoundGroups
	return r
}

// bound_groups takes every default-mode branch of Precheck.
func TestPrecheck_BoundGroupsModeActsLikeDefault(t *testing.T) {
	ok := config.ServiceConfig{Provider: "aws", Service: "ec2", Enabled: true, BoundIdentities: []string{"k1"}}
	assert.NoError(t, Precheck(planWith(ok, boundRamp(config.RampSchedule{Type: "weekly", PercentPerStep: 25}))))
	assert.NoError(t, Precheck(planWith(ok, boundRamp(config.RampSchedule{Type: "immediate", PercentPerStep: 100}))))
	err := Precheck(planWith(ok, boundRamp(config.RampSchedule{Type: "immediate", PercentPerStep: 50})))
	require.ErrorIs(t, err, ErrUnsupportedPlan)
	assert.ErrorContains(t, err, "immediate ramp")
}

// bound_groups keeps the default mode's percent_per_step bounds in sizing.
func TestValidateInputs_BoundGroupsKeepsPercentBounds(t *testing.T) {
	svc := &config.ServiceConfig{Provider: "aws", Service: "ec2", Term: 1, Payment: "no-upfront", Coverage: 80}
	for _, mode := range []config.RampAllocationMode{"", config.RampAllocationBoundGroups} {
		r := &ramp{mode: mode}
		for _, bad := range []float64{0, -1, 100.5} {
			_, err := r.validateInputs(svc, bad)
			assert.Error(t, err, "mode %q percent %v", mode, bad)
		}
		_, err := r.validateInputs(svc, 25)
		assert.NoError(t, err, "mode %q", mode)
	}
	_, err := (&ramp{mode: config.RampAllocationEqualCount}).validateInputs(svc, 0)
	assert.NoError(t, err, "equal_count ignores percent_per_step")
}
