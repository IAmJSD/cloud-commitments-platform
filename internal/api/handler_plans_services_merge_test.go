package api

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #934: a legacy PUT /api/plans/{id} replaced the plan's whole services
// map with the single provider/service entry the request describes, so a
// multi-service plan (or one keyed "aws:savings-plans-<type>" by migration
// 000040) lost its other services even on a bare rename.

func immediateRamp() config.RampSchedule {
	return config.RampSchedule{Type: "immediate", PercentPerStep: 100, TotalSteps: 1}
}

func multiServicePlan() *config.PurchasePlan {
	return &config.PurchasePlan{ID: spPlanID, Name: "old", Enabled: true, NotificationDaysBefore: 3, RampSchedule: immediateRamp(),
		Services: map[string]config.ServiceConfig{
			"aws/ec2": {Provider: "aws", Service: "ec2", Enabled: true, Term: 1, Payment: "all-upfront", Coverage: 70,
				IncludeRegions: []string{"us-east-1"}, MinCount: 2},
			"aws/rds": {Provider: "aws", Service: "rds", Enabled: true, Term: 3, Payment: "no-upfront", Coverage: 60},
		}}
}

func legacyEdit(service string) *PlanRequest {
	return &PlanRequest{Name: "renamed", Provider: "aws", Service: service, Term: 3, Payment: "partial-upfront",
		TargetCoverage: 90, RampSchedule: "immediate", NotificationDaysBefore: 3, Enabled: true}
}

func TestMergeUpdatedPlan_KeepsOtherServicesOnMultiServicePlan(t *testing.T) {
	plan, err := mergeUpdatedPlan(multiServicePlan(), legacyEdit("ec2"), spPlanID, false)
	require.NoError(t, err)

	require.Len(t, plan.Services, 2, "the untouched rds entry must survive an ec2 edit")
	assert.Equal(t, multiServicePlan().Services["aws/rds"], plan.Services["aws/rds"])

	ec2 := plan.Services["aws/ec2"]
	assert.Equal(t, 3, ec2.Term)
	assert.Equal(t, "partial-upfront", ec2.Payment)
	assert.Equal(t, 90.0, ec2.Coverage)
	// Filters the legacy form cannot express stay on the edited entry.
	assert.Equal(t, []string{"us-east-1"}, ec2.IncludeRegions)
	assert.Equal(t, 2, ec2.MinCount)
}

func TestMergeUpdatedPlan_ReusesColonKeyFromMigration000040(t *testing.T) {
	// Migration 000040 fanned "aws:savings-plans" out into four colon keys
	// carrying the umbrella's value object verbatim (Service "savings-plans").
	umbrella := config.ServiceConfig{Provider: "aws", Service: "savings-plans", Enabled: true, Term: 1, Payment: "no-upfront", Coverage: 80}
	existing := &config.PurchasePlan{ID: spPlanID, Name: "sp", Enabled: true, NotificationDaysBefore: 3, RampSchedule: immediateRamp(),
		Services: map[string]config.ServiceConfig{
			"aws:savings-plans-compute":     umbrella,
			"aws:savings-plans-ec2instance": umbrella,
			"aws:savings-plans-sagemaker":   umbrella,
			"aws:savings-plans-database":    umbrella,
		}}

	plan, err := mergeUpdatedPlan(existing, legacyEdit("savings-plans-compute"), spPlanID, false)
	require.NoError(t, err)

	require.Len(t, plan.Services, 4)
	assert.NotContains(t, plan.Services, "aws/savings-plans-compute", "the existing colon key must be reused, not duplicated")
	compute := plan.Services["aws:savings-plans-compute"]
	assert.Equal(t, "savings-plans-compute", compute.Service)
	assert.Equal(t, 3, compute.Term)
	assert.Equal(t, umbrella, plan.Services["aws:savings-plans-database"])
}

func TestMergeUpdatedPlan_RejectsUnknownServiceOnMultiServicePlan(t *testing.T) {
	// The single-service form cannot say which of several entries to
	// replace; silently adding or swapping one could buy a service the user
	// never meant to add, so refuse instead of guessing.
	_, err := mergeUpdatedPlan(multiServicePlan(), legacyEdit("elasticache"), spPlanID, false)
	ce, ok := IsClientError(err)
	require.True(t, ok, "want ClientError, got %T: %v", err, err)
	assert.Equal(t, 400, ce.code)
	assert.Contains(t, ce.Error(), "aws/elasticache")
}

func TestMergeUpdatedPlan_SingleServicePlanStillSwitchesService(t *testing.T) {
	existing := spPlan("ec2", immediateRamp())
	plan, err := mergeUpdatedPlan(existing, legacyEdit("rds"), spPlanID, false)
	require.NoError(t, err)
	require.Len(t, plan.Services, 1)
	assert.Contains(t, plan.Services, "aws/rds")
}

func TestMergeUpdatedPlan_NoServiceInRequestKeepsAllServices(t *testing.T) {
	plan, err := mergeUpdatedPlan(multiServicePlan(), &PlanRequest{Name: "renamed", RampSchedule: "immediate", Enabled: true}, spPlanID, false)
	require.NoError(t, err)
	assert.Equal(t, multiServicePlan().Services, plan.Services)
}

func TestMergeUpdatedPlan_DoesNotMutateExistingServices(t *testing.T) {
	existing := multiServicePlan()
	_, err := mergeUpdatedPlan(existing, legacyEdit("ec2"), spPlanID, false)
	require.NoError(t, err)
	assert.Equal(t, multiServicePlan().Services, existing.Services)
}

func TestHandler_updatePlan_RenameKeepsAllServices(t *testing.T) {
	ctx := context.Background()
	h, store := spHandler(t)
	store.On("GetPurchasePlan", ctx, spPlanID).Return(multiServicePlan(), nil)
	var stored *config.PurchasePlan
	store.On("UpdatePurchasePlan", ctx, mock.AnythingOfType("*config.PurchasePlan")).
		Run(func(args mock.Arguments) { stored = args.Get(1).(*config.PurchasePlan) }).Return(nil)

	body := `{"name":"renamed","provider":"aws","service":"rds","term":3,"payment":"no-upfront","target_coverage":60,"ramp_schedule":"immediate","notification_days_before":3,"enabled":true}`
	_, err := h.updatePlan(ctx, spReq(body), spPlanID)
	require.NoError(t, err)

	require.NotNil(t, stored)
	assert.Equal(t, "renamed", stored.Name)
	assert.Equal(t, multiServicePlan().Services, stored.Services)
}
