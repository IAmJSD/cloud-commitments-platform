package api

import (
	"math"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMergeUpdatedPlan_MaxStepCommitment(t *testing.T) {
	cap100, cap5 := 100.0, 5.0
	existing := &config.PurchasePlan{Name: "p", MaxStepCommitment: &cap100,
		RampSchedule: config.RampSchedule{Type: "immediate", PercentPerStep: 100, TotalSteps: 1}}

	t.Run("omitted on update keeps the current cap", func(t *testing.T) {
		plan, err := mergeUpdatedPlan(existing, &PlanRequest{Name: "p", RampSchedule: "immediate"}, "id")
		require.NoError(t, err)
		require.NotNil(t, plan.MaxStepCommitment)
		assert.Equal(t, 100.0, *plan.MaxStepCommitment)
	})
	t.Run("provided on update replaces it", func(t *testing.T) {
		plan, err := mergeUpdatedPlan(existing, &PlanRequest{Name: "p", RampSchedule: "immediate", MaxStepCommitment: &cap5}, "id")
		require.NoError(t, err)
		assert.Equal(t, 5.0, *plan.MaxStepCommitment)
	})
	for name, v := range map[string]float64{"zero": 0, "negative": -1, "NaN": math.NaN(), "Inf": math.Inf(1)} {
		t.Run("rejects "+name, func(t *testing.T) {
			bad := v
			_, err := mergeUpdatedPlan(existing, &PlanRequest{Name: "p", RampSchedule: "immediate", MaxStepCommitment: &bad}, "id")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "max_step_commitment")
		})
	}
}
