package api

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestMergeUpdatedPlan_MaxStepCommitment(t *testing.T) {
	cap100, cap5 := 100.0, 5.0
	existing := &config.PurchasePlan{Name: "p", MaxStepCommitment: &cap100,
		RampSchedule: config.RampSchedule{Type: "immediate", PercentPerStep: 100, TotalSteps: 1}}

	t.Run("omitted on update keeps the current cap", func(t *testing.T) {
		plan, err := mergeUpdatedPlan(existing, &PlanRequest{Name: "p", RampSchedule: "immediate"}, "id", false)
		require.NoError(t, err)
		require.NotNil(t, plan.MaxStepCommitment)
		assert.Equal(t, 100.0, *plan.MaxStepCommitment)
	})
	t.Run("provided on update replaces it", func(t *testing.T) {
		plan, err := mergeUpdatedPlan(existing, &PlanRequest{Name: "p", RampSchedule: "immediate", MaxStepCommitment: &cap5}, "id", true)
		require.NoError(t, err)
		assert.Equal(t, 5.0, *plan.MaxStepCommitment)
	})
	t.Run("explicit null clears a manual cap", func(t *testing.T) {
		plan, err := mergeUpdatedPlan(existing, &PlanRequest{Name: "p", RampSchedule: "immediate"}, "id", true)
		require.NoError(t, err)
		assert.Nil(t, plan.MaxStepCommitment)
		assert.Equal(t, 100.0, *existing.MaxStepCommitment)
	})
	t.Run("explicit null refuses automatic mode", func(t *testing.T) {
		plan, err := mergeUpdatedPlan(existing, &PlanRequest{Name: "p", RampSchedule: "immediate", AutoPurchase: true}, "id", true)
		require.ErrorContains(t, err, "cannot be cleared")
		assert.Nil(t, plan)
	})
	for name, v := range map[string]float64{"zero": 0, "negative": -1, "NaN": math.NaN(), "Inf": math.Inf(1)} {
		t.Run("rejects "+name, func(t *testing.T) {
			bad := v
			_, err := mergeUpdatedPlan(existing, &PlanRequest{Name: "p", RampSchedule: "immediate", MaxStepCommitment: &bad}, "id", true)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "max_step_commitment")
		})
	}
}

func TestUpdatePlan_CapNullKeepsAuthorization(t *testing.T) {
	for _, status := range []int{403, 404} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			ctx := context.Background()
			store := new(MockConfigStore)
			mockAuth := new(MockAuthService)
			t.Cleanup(func() { mockAuth.AssertExpectations(t) })
			mockAuth.On("ValidateSession", mock.Anything, "cap-token").Return(&Session{UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}, nil)
			mockAuth.On("ValidateCSRFToken", mock.Anything, mock.Anything, mock.Anything).Return(nil)
			if status == 403 {
				mockAuth.grantPermissions([]auth.Permission{{Action: auth.ActionView, Resource: auth.ResourcePlans}})
			} else {
				mockAuth.grantScoped("allowed-account")
				store.GetPlanAccountsFn = func(context.Context, string) ([]config.CloudAccount, error) {
					return []config.CloudAccount{{Name: "other-account"}}, nil
				}
			}
			h := &Handler{config: store, auth: mockAuth}
			req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer cap-token", "Content-Type": "application/json", "X-CSRF-Token": "test-csrf"}, Body: `{"name":"p","max_step_commitment":null}`, RequestContext: events.LambdaFunctionURLRequestContext{HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "PUT", Path: "/api/plans/11111111-1111-1111-1111-111111111111"}}}
			response, err := h.HandleRequest(ctx, req)
			require.NoError(t, err)
			assert.Equal(t, status, response.StatusCode, response.Body)
			store.AssertNotCalled(t, "GetPurchasePlan")
			store.AssertNotCalled(t, "UpdatePurchasePlan")
		})
	}
}
