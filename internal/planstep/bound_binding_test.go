package planstep

import (
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixtures are hand-built plans, not real configuration (issue #1746).

func boundPlan(marked bool, entries map[string]config.ServiceConfig) *config.PurchasePlan {
	p := &config.PurchasePlan{ID: "plan-bound", Services: entries, RampSchedule: config.RampSchedule{Type: "weekly", PercentPerStep: 25}}
	if marked {
		p.RampSchedule.AllocationMode = config.RampAllocationBoundGroups
	}
	return p
}

func boundEntry(ids ...string) config.ServiceConfig {
	return config.ServiceConfig{Provider: "aws", Service: "ec2", Enabled: true, MinCount: 1, GroupID: "g1", BoundIdentities: ids}
}

func TestPrecheck_BoundBinding(t *testing.T) {
	for _, tc := range []struct {
		name    string
		plan    *config.PurchasePlan
		missing bool   // want ErrBoundPlanMissingBinding
		want    string // other refusal substring; "" = accepted
	}{
		{"marked plan with bindings and the sentinel", boundPlan(true, map[string]config.ServiceConfig{"aws/ec2#g1": boundEntry("k1")}), false, ""},
		{"marked plan, entry lost its binding", boundPlan(true, map[string]config.ServiceConfig{"aws/ec2#g1": boundEntry()}), true, ""},
		{"#-keyed entry without marker or binding", boundPlan(false, map[string]config.ServiceConfig{"aws/ec2#g1": boundEntry()}), true, ""},
		{"marked plan, legacy key without binding", boundPlan(true, map[string]config.ServiceConfig{"aws/ec2": {Provider: "aws", Service: "ec2", Enabled: true}}), true, ""},
		{"disabled entry without binding is not checked", boundPlan(true, map[string]config.ServiceConfig{
			"aws/ec2#g1": boundEntry("k1"),
			"aws/rds#g2": {Provider: "aws", Service: "rds", MinCount: 1, GroupID: "g2"},
		}), false, ""},
		{"sentinel other than 1 on a #-key is still a filter", boundPlan(true, map[string]config.ServiceConfig{"aws/ec2#g1": withSvc(boundEntry("k1"), func(s *config.ServiceConfig) { s.MinCount = 2 })}), false, "min_count"},
		{"other filters on a #-key are still refused", boundPlan(true, map[string]config.ServiceConfig{"aws/ec2#g1": withSvc(boundEntry("k1"), func(s *config.ServiceConfig) { s.IncludeTypes = []string{"m5.large"} })}), false, "include_types"},
		// (guard) a legacy key keeps today's refusal of min_count 1.
		{"min_count 1 on a legacy key is refused", boundPlan(false, map[string]config.ServiceConfig{"aws/ec2": {Provider: "aws", Service: "ec2", Enabled: true, MinCount: 1}}), false, "min_count"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Precheck(tc.plan)
			switch {
			case tc.missing:
				require.ErrorIs(t, err, ErrBoundPlanMissingBinding)
				require.ErrorIs(t, err, ErrUnsupportedPlan, "attach must record a failed row, not retry")
				assert.ErrorContains(t, err, "plan-bound")
			case tc.want != "":
				require.ErrorIs(t, err, ErrUnsupportedPlan)
				assert.ErrorContains(t, err, tc.want)
			default:
				assert.NoError(t, err)
			}
		})
	}
}
