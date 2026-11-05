package api

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/planstep"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Fixtures are hand-built plans, not real configuration (issue #1746).

// strippedBoundPlan is a bound plan whose group fields an older binary's
// write removed: the "#" key and the min_count sentinel survive, the bound
// identities do not.
func strippedBoundPlan(armed bool) *config.PurchasePlan {
	p := apgStoredPlan(armed, apgCap(1000))
	p.Services = map[string]config.ServiceConfig{"aws/rds#g1": {Provider: "aws", Service: "rds", Enabled: true, MinCount: 1}}
	return p
}

func boundPatch(t *testing.T, stored *config.PurchasePlan, body string) (*MockConfigStore, error) {
	t.Helper()
	store := new(MockConfigStore)
	store.On("GetPurchasePlan", mock.Anything, apgPlanID).Return(stored, nil)
	store.On("GetPlanAccounts", mock.Anything, apgPlanID).Return([]config.CloudAccount{{ID: apsAcctA, Provider: "aws"}}, nil).Maybe()
	store.On("UpdatePurchasePlan", mock.Anything, mock.Anything).Return(nil).Maybe()
	store.On("UpdatePurchasePlanGated", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	h, req := apgCaller{perms: apsGrant(nil)}.handlerAndReq(store, body)
	_, err := h.patchPlan(context.Background(), req, apgPlanID)
	return store, err
}

func TestPlanArm_RefusesStrippedBoundPlan(t *testing.T) {
	store, err := boundPatch(t, strippedBoundPlan(false), `{"auto_purchase":true}`)
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "must be a client error, not a 500: %v", err)
	assert.Equal(t, 409, ce.code)
	assert.Contains(t, ce.Error(), planstep.ErrBoundPlanMissingBinding.Error())
	assert.False(t, apgCalled(store, "UpdatePurchasePlan"))
	assert.False(t, apgCalled(store, "UpdatePurchasePlanGated"))
}

// De-escalating writes must always pass, so an operator can stop a plan that
// lost its binding.
func TestPlanDisarm_AllowsStrippedBoundPlan(t *testing.T) {
	for _, body := range []string{`{"auto_purchase":false}`, `{"enabled":false}`, `{"name":"renamed"}`} {
		store, err := boundPatch(t, strippedBoundPlan(true), body)
		require.NoError(t, err, body)
		assert.True(t, apgCalled(store, "UpdatePurchasePlan"), body)
	}
}
