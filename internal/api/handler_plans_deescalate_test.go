package api

import (
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixtures are hand-built plans, not real configuration (issue #1746).

// conflictedBoundPlan is an armed bound plan whose stored groups fail the
// bound-group checks: one recommendation is bound twice.
func conflictedBoundPlan() *config.PurchasePlan {
	p := apgStoredPlan(true, apgCap(1000))
	p.RampSchedule.AllocationMode = config.RampAllocationBoundGroups
	p.Services = map[string]config.ServiceConfig{"aws/rds#g1": {
		Provider: "aws", Service: "rds", Enabled: true, Term: 1, Payment: "no-upfront", Coverage: 80,
		MinCount: 1, GroupID: "g1", BoundIdentities: []string{"k1", "k1"},
	}}
	return p
}

func TestPatchPlan_StoppingABoundPlanSkipsGroupChecks(t *testing.T) {
	for _, body := range []string{`{"auto_purchase":false}`, `{"enabled":false}`, `{"enabled":false,"name":"stopped"}`} {
		store, err := boundPatch(t, conflictedBoundPlan(), body)
		require.NoError(t, err, body)
		assert.True(t, apgCalled(store, "UpdatePurchasePlan"), body)
	}
	// A PATCH that turns anything on is not a stop, even if it also disarms:
	// re-enabling a disabled conflicted plan must run the group checks
	// (ui-fix-1's repro on #1774).
	disabled := conflictedBoundPlan()
	disabled.Enabled = false
	store, err := boundPatch(t, disabled, `{"enabled":true,"auto_purchase":false}`)
	ce, ok := IsClientError(err)
	require.True(t, ok, "must be a client error: %v", err)
	assert.Equal(t, 400, ce.code)
	assert.False(t, apgCalled(store, "UpdatePurchasePlan"))

	// Arming while disabling isn't a stop either.
	store, err = boundPatch(t, conflictedBoundPlan(), `{"enabled":false,"auto_purchase":true}`)
	ce, ok = IsClientError(err)
	require.True(t, ok, "must be a client error: %v", err)
	assert.Equal(t, 400, ce.code)
	assert.False(t, apgCalled(store, "UpdatePurchasePlan"))

	// Any other write still runs every check.
	store, err = boundPatch(t, conflictedBoundPlan(), `{"name":"renamed"}`)
	ce, ok = IsClientError(err)
	require.True(t, ok, "must be a client error: %v", err)
	assert.Equal(t, 400, ce.code)
	assert.False(t, apgCalled(store, "UpdatePurchasePlan"))
}
