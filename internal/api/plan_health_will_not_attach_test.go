package api

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// healthyRampPlan is a plan that, with no doomed root, scores 100 at now.
func healthyRampPlan(now time.Time, noticeDays int) config.PurchasePlan {
	next := now.Add(72 * time.Hour)
	return config.PurchasePlan{
		ID: "11111111-1111-1111-1111-111111111111", Name: "Azure compute", Enabled: true, NotificationDaysBefore: noticeDays,
		NextExecutionDate: &next,
		RampSchedule: config.RampSchedule{Type: "weekly", PercentPerStep: 25, StepIntervalDays: 7, TotalSteps: 4,
			StartDate: now.AddDate(0, 0, -1)},
	}
}

func TestComputePlanHealth_StepWillNotAttach(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

	t.Run("window note names the step and the notice days", func(t *testing.T) {
		score, factors := computePlanHealth(healthyRampPlan(now, 3), now, nil, config.RampStepBlock{}, config.DoomedRoot{StepNumber: 1})
		assert.Equal(t, 100-penaltyStepWillNotAttach, score)
		f := factorByCode(t, factors, HealthFactorStepWillNotAttach)
		assert.Equal(t, penaltyStepWillNotAttach, f.Penalty)
		assert.Equal(t, "Step 1 is scheduled inside the 3-day notice window, so it will not get recommendations and will fail on its date.", f.Note)
	})

	t.Run("no notice window gets its own note", func(t *testing.T) {
		_, factors := computePlanHealth(healthyRampPlan(now, 0), now, nil, config.RampStepBlock{}, config.DoomedRoot{StepNumber: 2})
		assert.Equal(t, "Step 2 will not get recommendations because the plan has no notice window (notification_days_before is 0).",
			factorByCode(t, factors, HealthFactorStepWillNotAttach).Note)
	})

	t.Run("nothing doomed adds nothing", func(t *testing.T) {
		score, factors := computePlanHealth(healthyRampPlan(now, 3), now, nil, config.RampStepBlock{}, config.DoomedRoot{})
		assert.Equal(t, 100, score)
		assert.Empty(t, factors)
	})
}

// Every factor collectPlanHealthFactors can emit must be in planHealthFactorCodes,
// the slice the openapi enum is compared with.
func TestPlanHealthFactorCodes_CoverEveryEmittedFactor(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	past := now.AddDate(0, 0, -2)
	emitted := map[PlanHealthFactorCode]bool{}
	collect := func(plan config.PurchasePlan, block config.RampStepBlock) {
		for _, f := range collectPlanHealthFactors(plan, now, counts(map[string]int{"failed": 1, "canceled": 1}), block, config.DoomedRoot{StepNumber: 1}) {
			emitted[f.Code] = true
		}
	}
	blocked := healthyRampPlan(now, 3)
	blocked.NextExecutionDate = &past
	collect(blocked, config.RampStepBlock{StepNumber: 1, StuckExecutions: 1})
	stalled := healthyRampPlan(now, 3)
	stalled.RampSchedule.StartDate = now.AddDate(0, 0, -30)
	collect(stalled, config.RampStepBlock{})
	behind := stalled
	behind.RampSchedule.CurrentStep = 1
	collect(behind, config.RampStepBlock{})
	midway := behind
	midway.Enabled = false
	collect(midway, config.RampStepBlock{})

	known := map[PlanHealthFactorCode]bool{}
	for _, c := range planHealthFactorCodes {
		known[c] = true
	}
	for code := range emitted {
		assert.True(t, known[code], "factor %q is emitted but missing from planHealthFactorCodes", code)
	}
	assert.Len(t, emitted, len(planHealthFactorCodes), "every declared code is emitted by some plan shape")
}

func TestOpenAPIPlanHealthFactorEnumMatchesGoCodes(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "openapi.yaml"))
	require.NoError(t, err)
	var doc struct {
		Components struct {
			Schemas struct {
				PlanHealthFactor struct {
					Properties struct {
						Code struct {
							Enum []string `yaml:"enum"`
						} `yaml:"code"`
					} `yaml:"properties"`
				} `yaml:"PlanHealthFactor"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	require.NoError(t, yaml.Unmarshal(data, &doc))
	want := make([]string, len(planHealthFactorCodes))
	for i, c := range planHealthFactorCodes {
		want[i] = string(c)
	}
	require.ElementsMatch(t, want, doc.Components.Schemas.PlanHealthFactor.Properties.Code.Enum)
}

// The 2026-11-04 scenario of #631: an Azure compute plan (1y, 80%, weekly 25%,
// one account) whose next root was added 24h out with a 3-day notice window
// showed health 100 and then failed on its date. listPlans must now score it
// down and name the factor. The store is mocked at the read; the clock is the
// handler's, so `now` is pinned.
func TestHandler_listPlans_DoomedRootProducesWillNotAttachFactor(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	mockStore := new(MockConfigStore)
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockAuth.On("ValidateSession", ctx, "admin-token").Return(&Session{UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Email: "admin@example.com"}, nil)
	mockAuth.grantAdmin()

	doomed := healthyRampPlan(now, 3)
	doomed.Services = map[string]config.ServiceConfig{"compute": {Provider: "azure", Service: "compute", Enabled: true, Term: 1, Payment: "upfront", Coverage: 80}}
	fine := healthyRampPlan(now, 3)
	fine.ID, fine.Name = "22222222-2222-2222-2222-222222222222", "Fine plan"
	mockStore.On("ListPurchasePlans", ctx, config.PurchasePlanFilter{IncludeUnassigned: true}).Return([]config.PurchasePlan{doomed, fine}, nil)
	mockStore.On("CountExecutionsByPlanAndStatus", ctx, planHealthExecutionStatuses, mock.AnythingOfType("time.Time")).
		Return(map[string]config.ExecutionStatusCounts{}, nil)
	mockStore.On("GetDoomedNextRoots", ctx, now).Return(map[string]config.DoomedRoot{doomed.ID: {StepNumber: 1}}, nil)

	handler := &Handler{config: mockStore, auth: mockAuth, now: func() time.Time { return now }}
	req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer admin-token"}}
	result, err := handler.listPlans(ctx, req, map[string]string{})
	require.NoError(t, err)
	require.Len(t, result.Plans, 2)

	byID := map[string]PlanWithHealth{}
	for _, p := range result.Plans {
		byID[p.ID] = p
	}
	require.NotNil(t, byID[doomed.ID].HealthScore)
	assert.Equal(t, 75, *byID[doomed.ID].HealthScore)
	require.Len(t, byID[doomed.ID].HealthFactors, 1)
	assert.Equal(t, HealthFactorStepWillNotAttach, byID[doomed.ID].HealthFactors[0].Code)
	require.NotNil(t, byID[fine.ID].HealthScore)
	assert.Equal(t, 100, *byID[fine.ID].HealthScore)
	assert.Empty(t, byID[fine.ID].HealthFactors)
}

func TestHandler_listPlans_DoomedRootFetchFailureLeavesHealthUnknown(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockAuth.On("ValidateSession", ctx, "admin-token").Return(&Session{UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Email: "admin@example.com"}, nil)
	mockAuth.grantAdmin()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	mockStore.On("ListPurchasePlans", ctx, config.PurchasePlanFilter{IncludeUnassigned: true}).Return([]config.PurchasePlan{healthyRampPlan(now, 3)}, nil)
	mockStore.On("CountExecutionsByPlanAndStatus", ctx, planHealthExecutionStatuses, mock.AnythingOfType("time.Time")).
		Return(map[string]config.ExecutionStatusCounts{}, nil)
	mockStore.On("GetDoomedNextRoots", ctx, now).Return(nil, assert.AnError)

	handler := &Handler{config: mockStore, auth: mockAuth, now: func() time.Time { return now }}
	req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer admin-token"}}
	result, err := handler.listPlans(ctx, req, map[string]string{})
	require.NoError(t, err)
	require.Len(t, result.Plans, 1)
	assert.Nil(t, result.Plans[0].HealthScore, "a score blind to doomed roots would err healthy")
}
