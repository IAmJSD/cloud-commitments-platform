package api

import (
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixtures are hand-built plans, not real configuration (issue #1746).

// A PUT before step 1 may change the ramp shape, but a bound plan must keep
// its bound_groups marker: dropping it would let the plan buy by filter.
func TestUpdatePlan_KeepsBoundGroupsMarker(t *testing.T) {
	const planID = "12345678-1234-1234-1234-123456789abc"
	weekly := config.PresetRampSchedules[config.RampWeekly25Pct]
	for _, tc := range []struct {
		name, body string
		stored     config.RampAllocationMode
		want       config.RampAllocationMode
	}{
		{"shape change keeps bound_groups", `{"name":"p","ramp_schedule":"monthly-10pct"}`, config.RampAllocationBoundGroups, config.RampAllocationBoundGroups},
		// (guard) the same shape already carried every mode.
		{"same shape keeps bound_groups", `{"name":"renamed","ramp_schedule":"weekly-25pct"}`, config.RampAllocationBoundGroups, config.RampAllocationBoundGroups},
		// (guard) other modes still reset on a shape change, as today.
		{"shape change still drops equal_count", `{"name":"p","ramp_schedule":"monthly-10pct"}`, config.RampAllocationEqualCount, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := weekly
			rs.AllocationMode = tc.stored
			existing := &config.PurchasePlan{ID: planID, Name: "p", Enabled: true, RampSchedule: rs}
			resp, saved := putPlan(t, existing, tc.body)
			require.Equal(t, 200, resp.StatusCode, resp.Body)
			require.NotNil(t, saved)
			assert.Equal(t, tc.want, saved.RampSchedule.AllocationMode)
		})
	}
}
