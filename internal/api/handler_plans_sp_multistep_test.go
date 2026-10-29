package api

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Savings Plans are scoped out of multi-step ramps (platform#631 Q7): create,
// PUT and PATCH (arm) refuse an armed multi-step plan with an SP service with
// a 400, and leave single-step SP and multi-step non-SP plans alone.

const spPlanID = "11111111-1111-1111-1111-111111111111"

func spHandler(t *testing.T) (*Handler, *MockConfigStore) {
	t.Helper()
	ctx := context.Background()
	store := new(MockConfigStore)
	auth := new(MockAuthService)
	t.Cleanup(func() { auth.AssertExpectations(t) })
	auth.On("ValidateSession", ctx, "admin-token").Return(&Session{UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Email: "admin@example.com"}, nil)
	auth.grantAdmin()
	return &Handler{config: store, auth: auth}, store
}

func spReq(body string) *events.LambdaFunctionURLRequest {
	return &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer admin-token"}, Body: body}
}

func requireSP400(t *testing.T, err error) {
	t.Helper()
	ce, ok := IsClientError(err)
	require.True(t, ok, "want ClientError, got %T: %v", err, err)
	assert.Equal(t, 400, ce.code)
	assert.Contains(t, ce.Error(), "multi-step ramps do not support Savings Plans")
}

func TestCreatePlan_RejectsArmedMultiStepSavingsPlan(t *testing.T) {
	h, _ := spHandler(t)
	body := `{"name":"P","provider":"aws","service":"savingsplans","ramp_schedule":"weekly-25pct","enabled":true,"auto_purchase":true,"target_accounts":["bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"]}`
	_, err := h.createPlan(context.Background(), spReq(body))
	requireSP400(t, err)
}

func TestMergeUpdatedPlan_RejectsArmedMultiStepSavingsPlan(t *testing.T) {
	existing := &config.PurchasePlan{Name: "p", RampSchedule: config.RampSchedule{Type: "immediate", PercentPerStep: 100, TotalSteps: 1}}
	_, err := mergeUpdatedPlan(existing, &PlanRequest{Name: "p", Provider: "aws", Service: "savingsplans",
		RampSchedule: "weekly-25pct", Enabled: true, AutoPurchase: true}, "id", false)
	requireSP400(t, err)
}

// patchSP expects the plan write only when writes is true: a refused PATCH
// must not reach UpdatePurchasePlan, and an unexpected call fails the test.
func patchSP(t *testing.T, existing *config.PurchasePlan, body string, writes bool) (any, error) {
	t.Helper()
	ctx := context.Background()
	h, store := spHandler(t)
	t.Cleanup(func() { store.AssertExpectations(t) })
	store.On("GetPurchasePlan", ctx, spPlanID).Return(existing, nil)
	if writes {
		store.On("UpdatePurchasePlan", ctx, mock.AnythingOfType("*config.PurchasePlan")).Return(nil)
	}
	return h.patchPlan(ctx, spReq(body), spPlanID)
}

func spPlan(service string, ramp config.RampSchedule) *config.PurchasePlan {
	return &config.PurchasePlan{ID: spPlanID, Name: "p", NotificationDaysBefore: 3, RampSchedule: ramp,
		Services: map[string]config.ServiceConfig{"aws/" + service: {Provider: "aws", Service: service, Enabled: true, Term: 3, Payment: "no-upfront", Coverage: 80}}}
}

func TestPatchPlan_ArmingMultiStepSavingsPlanIs400(t *testing.T) {
	weekly := config.RampSchedule{Type: "weekly", PercentPerStep: 25, StepIntervalDays: 7, TotalSteps: 4}
	_, err := patchSP(t, spPlan("savingsplans", weekly), `{"enabled":true,"auto_purchase":true}`, false)
	requireSP400(t, err)
}

func TestPatchPlan_SingleStepSavingsPlanAndMultiStepNonSPArmFine(t *testing.T) {
	immediate := config.RampSchedule{Type: "immediate", PercentPerStep: 100, TotalSteps: 1}
	weekly := config.RampSchedule{Type: "weekly", PercentPerStep: 25, StepIntervalDays: 7, TotalSteps: 4}
	for name, plan := range map[string]*config.PurchasePlan{
		"single-step SP":          spPlan("savingsplans", immediate),
		"multi-step RDS":          spPlan("rds", weekly),
		"single custom SP (100%)": spPlan("savingsplans", config.RampSchedule{Type: "custom", PercentPerStep: 100, StepIntervalDays: 7, TotalSteps: 1}),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := patchSP(t, plan, `{"enabled":true,"auto_purchase":true}`, true)
			require.NoError(t, err)
		})
	}
}

func TestPatchPlan_UnarmedMultiStepSavingsPlanIsAllowed(t *testing.T) {
	weekly := config.RampSchedule{Type: "weekly", PercentPerStep: 25, StepIntervalDays: 7, TotalSteps: 4}
	_, err := patchSP(t, spPlan("savingsplans", weekly), `{"name":"renamed"}`, true)
	require.NoError(t, err)
}
