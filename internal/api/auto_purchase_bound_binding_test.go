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

// Re-enabling a stripped entry widens an armed plan even though
// planArmEscalates cannot see it (its service sets ignore Enabled).
func TestUpdatePlanGated_RefusesReenablingStrippedEntry(t *testing.T) {
	before := strippedBoundPlan(true)
	before.Services = map[string]config.ServiceConfig{
		"aws/ec2#g1": {Provider: "aws", Service: "ec2", Enabled: true, MinCount: 1, GroupID: "g1", BoundIdentities: []string{"k1"}},
		"aws/rds#g2": {Provider: "aws", Service: "rds", Enabled: false, MinCount: 1},
	}
	after := *before
	after.Services = map[string]config.ServiceConfig{"aws/ec2#g1": before.Services["aws/ec2#g1"], "aws/rds#g2": before.Services["aws/rds#g2"]}
	g2 := after.Services["aws/rds#g2"]
	g2.Enabled = true
	after.Services["aws/rds#g2"] = g2
	require.False(t, planArmEscalates(before, &after), "the gap: the arming gate alone misses this write")

	store := new(MockConfigStore)
	h, req := apgCaller{perms: apsGrant(nil)}.handlerAndReq(store, `{}`)
	err := h.updatePlanGated(context.Background(), req, &Session{UserID: apgUserID}, before, &after, "updatePlan")
	ce, ok := IsClientError(err)
	require.True(t, ok, "must be a client error: %v", err)
	assert.Equal(t, 409, ce.code)
	assert.False(t, apgCalled(store, "UpdatePurchasePlan"))

	// (guard) re-enabling a bound entry that kept its binding still saves.
	g2.BoundIdentities = []string{"k2"}
	after.Services["aws/rds#g2"] = g2
	store.On("UpdatePurchasePlan", mock.Anything, mock.Anything).Return(nil)
	require.NoError(t, h.updatePlanGated(context.Background(), req, &Session{UserID: apgUserID}, before, &after, "updatePlan"))
}

func TestRetargetArmedPlan_RefusesStrippedBoundPlan(t *testing.T) {
	store, err := apsSetAccounts(t, apsGrant(nil), strippedBoundPlan(true), apsAcctB)
	ce, ok := IsClientError(err)
	require.True(t, ok, "must be a client error: %v", err)
	assert.Equal(t, 409, ce.code)
	assert.False(t, apgCalled(store, "SetPlanAccounts"))

	// (guard) an unarmed stripped plan can still be retargeted.
	store, err = apsSetAccounts(t, apsGrant(nil), strippedBoundPlan(false), apsAcctB)
	require.NoError(t, err)
	assert.True(t, apgCalled(store, "SetPlanAccounts"))
}

// (guard) The re-enable gate applies only while the plan stays armed: an
// unarmed stripped plan can still re-enable an entry.
func TestUpdatePlanGated_UnarmedReenableIsAccepted(t *testing.T) {
	before := strippedBoundPlan(false)
	before.Services = map[string]config.ServiceConfig{"aws/rds#g2": {Provider: "aws", Service: "rds", Enabled: false, MinCount: 1}}
	after := *before
	after.Services = map[string]config.ServiceConfig{"aws/rds#g2": {Provider: "aws", Service: "rds", Enabled: true, MinCount: 1}}
	store := new(MockConfigStore)
	store.On("UpdatePurchasePlan", mock.Anything, mock.Anything).Return(nil)
	h, req := apgCaller{perms: apsGrant(nil)}.handlerAndReq(store, `{}`)
	require.NoError(t, h.updatePlanGated(context.Background(), req, &Session{UserID: apgUserID}, before, &after, "updatePlan"))
	assert.True(t, apgCalled(store, "UpdatePurchasePlan"))
}

// A new entry counts as enabled: an armed bound plan gaining a second group of a
// service it already has (DerivePlanServices unchanged, so no escalation) is
// refused when that group has no binding.
func TestUpdatePlanGated_RefusesNewUnboundGroupOnArmedPlan(t *testing.T) {
	before := strippedBoundPlan(true)
	before.RampSchedule.AllocationMode = config.RampAllocationBoundGroups
	before.Services = map[string]config.ServiceConfig{
		"aws/ec2#g1": {Provider: "aws", Service: "ec2", Enabled: true, MinCount: 1, GroupID: "g1", BoundIdentities: []string{"k1"}},
	}
	after := *before
	after.Services = map[string]config.ServiceConfig{
		"aws/ec2#g1": before.Services["aws/ec2#g1"],
		"aws/ec2#g2": {Provider: "aws", Service: "ec2", Enabled: true, MinCount: 1, GroupID: "g2"},
	}
	// Pinned directly: once DerivePlanServices drops the "#" suffix (slice A
	// step A2), this gate is the only save-time refusal for a new group.
	require.True(t, enablesEntry(before, &after), "a newly added enabled entry counts as enabling")
	store := new(MockConfigStore)
	h, req := apgCaller{perms: apsGrant(nil)}.handlerAndReq(store, `{}`)
	err := h.updatePlanGated(context.Background(), req, &Session{UserID: apgUserID}, before, &after, "updatePlan")
	ce, ok := IsClientError(err)
	require.True(t, ok, "must be a client error: %v", err)
	assert.Equal(t, 409, ce.code)
	assert.False(t, apgCalled(store, "UpdatePurchasePlan"))
}
