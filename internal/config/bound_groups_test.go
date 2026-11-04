package config

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixtures are hand-built configs, not real plans (issue #1746).

func TestValidateAllocationMode_BoundGroups(t *testing.T) {
	for _, r := range []RampSchedule{
		{Type: "immediate", PercentPerStep: 100, TotalSteps: 1},
		{Type: "weekly", PercentPerStep: 25, TotalSteps: 4},
		{Type: "custom", PercentPerStep: 10},
	} {
		r.AllocationMode = RampAllocationBoundGroups
		assert.NoError(t, r.ValidateAllocationMode(), "%+v", r)
		assert.NoError(t, r.Validate(), "%+v", r)
	}
}

func TestRampAllocationMode_SizesByPercent(t *testing.T) {
	assert.True(t, RampAllocationMode("").SizesByPercent())
	assert.True(t, RampAllocationBoundGroups.SizesByPercent())
	assert.False(t, RampAllocationEqualCount.SizesByPercent())
}

// (guard) bound_groups sizes like the default mode, never like equal_count.
func TestCumulativePercent_BoundGroupsMatchesDefault(t *testing.T) {
	def := RampSchedule{Type: "weekly", PercentPerStep: 30, TotalSteps: 3}
	bound := def
	bound.AllocationMode = RampAllocationBoundGroups
	for step := 0; step <= 4; step++ {
		assert.InDelta(t, def.CumulativePercent(step), bound.CumulativePercent(step), 1e-12, "step %d", step)
	}
}

func TestServiceConfig_GroupFieldsJSON(t *testing.T) {
	// (guard) a legacy entry keeps today's bytes: no group keys appear.
	legacy, err := json.Marshal(ServiceConfig{Provider: "aws", Service: "ec2", Enabled: true, Term: 1, Payment: "no-upfront", Coverage: 80})
	require.NoError(t, err)
	assert.JSONEq(t, `{"provider":"aws","service":"ec2","enabled":true,"term":1,"payment":"no-upfront","coverage":80,"ramp_schedule":""}`, string(legacy))

	cap := 250.5
	bound := ServiceConfig{Provider: "aws", Service: "ec2", GroupID: "g1", BoundIdentities: []string{"k1", "k2"}, MaxStepCommitment: &cap}
	raw, err := json.Marshal(bound)
	require.NoError(t, err)
	var back ServiceConfig
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, bound, back)
	assert.Contains(t, string(raw), `"group_id":"g1"`)
	assert.Contains(t, string(raw), `"bound_identities":["k1","k2"]`)
	assert.Contains(t, string(raw), `"max_step_commitment":250.5`)
}
