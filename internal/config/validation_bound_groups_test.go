package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixtures are hand-built plans, not real configuration (issue #1746).

func boundGroupsPlan(entries map[string]ServiceConfig) *PurchasePlan {
	return &PurchasePlan{
		Name: "bound", Enabled: true, Services: entries,
		RampSchedule: RampSchedule{Type: "weekly", PercentPerStep: 25, StepIntervalDays: 7, TotalSteps: 4, AllocationMode: RampAllocationBoundGroups},
	}
}

func groupEntry(gid, service string, term int, payment string, ids ...string) ServiceConfig {
	return ServiceConfig{Provider: "aws", Service: service, Enabled: true, Term: term, Payment: payment, Coverage: 80,
		MinCount: 1, GroupID: gid, BoundIdentities: ids}
}

func TestValidate_BoundGroups(t *testing.T) {
	ec2a := groupEntry("g1", "ec2", 1, "no-upfront", "k1", "k2")
	ec2b := groupEntry("g2", "ec2", 3, "all-upfront", "k3")
	capOf := func(s ServiceConfig, v float64) ServiceConfig { s.MaxStepCommitment = &v; return s }
	many := make([]string, MaxBoundIdentitiesPerGroup+1)
	for i := range many {
		many[i] = "k" + string(rune('a'+i%26)) + string(rune('0'+i/26%10)) + string(rune('0'+i/260))
	}
	for _, tc := range []struct {
		name    string
		entries map[string]ServiceConfig
		want    string // substring of the error; "" = valid
	}{
		{"two groups differing in term", map[string]ServiceConfig{"aws/ec2#g1": ec2a, "aws/ec2#g2": ec2b}, ""},
		{"two groups differing only in payment", map[string]ServiceConfig{"aws/ec2#g1": ec2a, "aws/ec2#g2": groupEntry("g2", "ec2", 1, "all-upfront", "k3")}, ""},
		{"two groups differing only in service", map[string]ServiceConfig{"aws/ec2#g1": ec2a, "aws/rds#g2": groupEntry("g2", "rds", 1, "no-upfront", "k3")}, ""},
		{"two groups differing only in provider", map[string]ServiceConfig{"azure/compute#g1": azureEntry("g1", "monthly", "k1"), "gcp/compute#g2": gcpEntry("g2", "k2")}, ""},
		{"two groups differing only in term", map[string]ServiceConfig{"aws/ec2#g1": ec2a, "aws/ec2#g2": groupEntry("g2", "ec2", 3, "no-upfront", "k3")}, ""},
		{"duplicate group id", map[string]ServiceConfig{"aws/ec2#g1": ec2a, "aws/rds#g1": groupEntry("g1", "rds", 3, "all-upfront", "k3")}, `group "g1"`},
		{"key suffix differs from group_id", map[string]ServiceConfig{"aws/ec2#g1": withGroup(ec2a, "g2")}, `must be keyed`},
		{"bound entry without a # key", map[string]ServiceConfig{"aws/ec2": ec2a}, `must be keyed`},
		{"bound entry without a group_id", map[string]ServiceConfig{"aws/ec2#g1": withGroup(ec2a, "")}, `must be keyed`},
		{"exactly 1000 identities is accepted", map[string]ServiceConfig{"aws/ec2#g1": groupEntry("g1", "ec2", 1, "no-upfront", many[:MaxBoundIdentitiesPerGroup]...)}, ""},
		{"duplicate identity", map[string]ServiceConfig{"aws/ec2#g1": ec2a, "aws/ec2#g2": groupEntry("g2", "ec2", 1, "no-upfront", "k3")}, "same provider, service, term and payment"},
		{"same identity on another provider", map[string]ServiceConfig{"azure/compute#g1": azureEntry("g1", "monthly", "k1"), "azure/compute#g2": azureEntry("g2", "monthly", "k2")}, "same provider, service, term and payment"},
		{"disabled duplicate identity is fine", map[string]ServiceConfig{"aws/ec2#g1": ec2a, "aws/ec2#g2": disabled(groupEntry("g2", "ec2", 1, "no-upfront", "k3"))}, ""},
		{"identity bound twice in a group", map[string]ServiceConfig{"aws/ec2#g1": groupEntry("g1", "ec2", 1, "no-upfront", "k1", "k1")}, "k1"},
		{"identity bound in two groups", map[string]ServiceConfig{"aws/ec2#g1": ec2a, "aws/ec2#g2": groupEntry("g2", "ec2", 3, "all-upfront", "k2")}, "k2"},
		{"too many identities", map[string]ServiceConfig{"aws/ec2#g1": groupEntry("g1", "ec2", 1, "no-upfront", many...)}, "at most 1000"},
		{"valid group cap", map[string]ServiceConfig{"aws/ec2#g1": capOf(ec2a, 250.5)}, ""},
		{"group cap below a cent", map[string]ServiceConfig{"aws/ec2#g1": capOf(ec2a, 0.001)}, "max_step_commitment"},
		{"group cap with three decimals", map[string]ServiceConfig{"aws/ec2#g1": capOf(ec2a, 1.005)}, "max_step_commitment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := boundGroupsPlan(tc.entries).Validate()
			if tc.want == "" {
				assert.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrInvalidBoundGroup)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

// The effective cap is min(group, plan): a plan cap below a group cap is valid.
func TestValidate_BoundGroups_PlanCapBelowGroupCap(t *testing.T) {
	g := groupEntry("g1", "ec2", 1, "no-upfront", "k1")
	groupCap, planCap := 500.0, 100.0
	g.MaxStepCommitment = &groupCap
	p := boundGroupsPlan(map[string]ServiceConfig{"aws/ec2#g1": g})
	p.MaxStepCommitment = &planCap
	assert.NoError(t, p.Validate())
}

// (guard) Unmarked plans validate exactly as today, including migration
// 000040's four aws:savings-plans-* siblings that share one identity.
func TestValidate_UnmarkedPlansSkipBoundGroupChecks(t *testing.T) {
	sp := func(service string) ServiceConfig {
		return ServiceConfig{Provider: "aws", Service: service, Enabled: true, Term: 1, Payment: "no-upfront", Coverage: 80}
	}
	p := &PurchasePlan{Name: "legacy", Enabled: true, RampSchedule: RampSchedule{Type: "immediate", PercentPerStep: 100, TotalSteps: 1},
		Services: map[string]ServiceConfig{
			"aws:savings-plans-compute": sp("savings-plans"), "aws:savings-plans-ec2instance": sp("savings-plans"),
			"aws:savings-plans-sagemaker": sp("savings-plans"), "aws:savings-plans-database": sp("savings-plans"),
		}}
	assert.NoError(t, p.Validate())
	p.Services["aws:dup"] = ServiceConfig{Provider: "aws", Service: "ec2", Enabled: true, Term: 1, Payment: "no-upfront", Coverage: 80, GroupID: "x", BoundIdentities: []string{"k1", "k1"}}
	assert.NoError(t, p.Validate(), "no marker: group fields are not validated here (Precheck guards buying)")
}

func TestServiceConfig_EffectiveGroupID(t *testing.T) {
	assert.Equal(t, "g1", (&ServiceConfig{GroupID: "g1"}).EffectiveGroupID("aws/ec2#g1"))
	assert.Equal(t, "aws/ec2", (&ServiceConfig{}).EffectiveGroupID("aws/ec2"))
}

func azureEntry(gid, payment string, ids ...string) ServiceConfig {
	e := groupEntry(gid, "compute", 1, payment, ids...)
	e.Provider = "azure"
	return e
}

func withGroup(s ServiceConfig, gid string) ServiceConfig { s.GroupID = gid; return s }
func disabled(s ServiceConfig) ServiceConfig              { s.Enabled = false; return s }

func gcpEntry(gid string, ids ...string) ServiceConfig {
	e := groupEntry(gid, "compute", 1, "monthly", ids...)
	e.Provider = "gcp"
	return e
}

// A write that only stops the plan skips the bound-group checks, and keeps
// every other check.
func TestValidateDeescalation_SkipsOnlyBoundGroupChecks(t *testing.T) {
	g := groupEntry("g1", "ec2", 1, "no-upfront", "k1", "k1")
	p := boundGroupsPlan(map[string]ServiceConfig{"aws/ec2#g1": g})
	require.ErrorIs(t, p.Validate(), ErrInvalidBoundGroup)
	assert.NoError(t, p.ValidateDeescalation())
	p.Name = ""
	assert.ErrorContains(t, p.ValidateDeescalation(), "plan name is required")
}
