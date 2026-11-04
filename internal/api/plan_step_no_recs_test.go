package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// The reported plan uses Azure compute, one year, monthly payment and a weekly ramp.

const (
	bareStepAPIPlanID = "11111111-1111-1111-1111-111111111609"
	bareStepAPIExecID = "22222222-3333-4444-5555-666666666609"
)

func bareStepAPIPlan() *config.PurchasePlan {
	return &config.PurchasePlan{
		ID:                     bareStepAPIPlanID,
		Name:                   "Azure compute 80% weekly",
		Enabled:                true,
		AutoPurchase:           true,
		NotificationDaysBefore: 2,
		Services: map[string]config.ServiceConfig{
			"azure:compute": {Provider: "azure", Service: "compute", Enabled: true, Term: 1, Payment: "monthly", Coverage: 80},
		},
		RampSchedule: config.RampSchedule{Type: "weekly", PercentPerStep: 25, CurrentStep: 0, TotalSteps: 4, StepIntervalDays: 7},
	}
}

func TestAddPurchasesToPlanCreatesFutureRoots(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	mockAuth.On("ValidateSession", ctx, "admin-token").Return(&Session{
		UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		Email:  "admin@example.com",
	}, nil)
	mockAuth.grantAdmin()
	plan := bareStepAPIPlan()
	plan.AutoPurchase = false
	mockStore.On("GetPurchasePlan", ctx, bareStepAPIPlanID).Return(plan, nil)
	mockStore.On("GetPlanAccounts", ctx, bareStepAPIPlanID).Return([]config.CloudAccount{{ID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", Provider: "azure"}}, nil)
	mockStore.On("SavePurchaseExecution", ctx, mock.Anything).Return(nil).Times(4)
	mockStore.On("UpdatePurchasePlan", ctx, mock.Anything).Return(nil)

	handler := &Handler{config: mockStore, auth: mockAuth}
	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"Authorization": "Bearer admin-token"},
		Body:    `{"count": 4, "start_date": "` + time.Now().AddDate(0, 0, 7).Format("2006-01-02") + `"}`,
	}

	result, err := handler.createPlannedPurchases(ctx, req, bareStepAPIPlanID)

	require.NoError(t, err)
	require.Equal(t, 4, result.Created)
	mockStore.AssertExpectations(t)
}

func TestAddPurchasesValidatesLockedPlan(t *testing.T) {
	tests := []struct {
		name     string
		change   func(*config.PurchasePlan)
		switchOn bool
		want     string
	}{
		{"disabled", func(p *config.PurchasePlan) { p.Enabled = false }, true, "enabled"},
		{"missing auto cap", func(p *config.PurchasePlan) { p.MaxStepCommitment = nil }, true, "max_step_commitment"},
		{"invalid cap", func(p *config.PurchasePlan) { v := 1.005; p.MaxStepCommitment = &v }, true, "decimal"},
		{"switch off", func(*config.PurchasePlan) {}, false, "disabled globally"},
		{"complete", func(p *config.PurchasePlan) { p.RampSchedule.CurrentStep = 4 }, true, "remaining"},
		{"negative step", func(p *config.PurchasePlan) { p.RampSchedule.CurrentStep = -1 }, true, "negative"},
		{"no interval", func(p *config.PurchasePlan) { p.RampSchedule.StepIntervalDays = 0 }, true, "positive"},
		{"no veto", func(p *config.PurchasePlan) { p.NotificationDaysBefore = 0 }, true, "veto"},
		{"ignored filter", func(p *config.PurchasePlan) {
			s := p.Services["azure:compute"]
			s.IncludeRegions = []string{"eastus"}
			p.Services["azure:compute"] = s
		}, true, "include_regions"},
		{"automatic", func(*config.PurchasePlan) {}, true, ""},
		{"manual switch off", func(p *config.PurchasePlan) { p.AutoPurchase = false; p.MaxStepCommitment = nil }, false, ""},
		{"immediate", func(p *config.PurchasePlan) {
			p.RampSchedule = config.RampSchedule{Type: "immediate", PercentPerStep: 100, TotalSteps: 1}
		}, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := new(MockConfigStore)
			t.Cleanup(func() { store.AssertExpectations(t) })
			plan := bareStepAPIPlan()
			cap := 100.0
			plan.MaxStepCommitment = &cap
			tt.change(plan)
			store.On("GetPurchasePlan", ctx, plan.ID).Return(plan, nil)
			store.On("GetPlanAccounts", ctx, plan.ID).Return([]config.CloudAccount{{ID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", Provider: "azure"}}, nil).Maybe()
			store.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{AutoPurchaseEnabled: tt.switchOn}, nil).Maybe()
			if tt.want == "" {
				store.On("SavePurchaseExecution", ctx, mock.Anything).Return(nil).Once()
				store.On("UpdatePurchasePlan", ctx, mock.Anything).Return(nil).Once()
			}
			h := &Handler{config: store}
			first := time.Now().AddDate(0, 0, 7)
			n, err := h.createPlannedPurchasesTx(ctx, nil, plan.ID, 1, first, nil, &Session{UserID: apiKeyAdminUserID})
			if tt.want != "" {
				require.ErrorContains(t, err, tt.want)
				assert.Zero(t, n)
				store.AssertNotCalled(t, "SavePurchaseExecutionTx")
			} else {
				require.NoError(t, err)
				assert.Equal(t, 1, n)
				require.True(t, first.Equal(*plan.NextExecutionDate))
			}
		})
	}
}

func TestAddPurchasesAnchorsPartlyCompletedRamp(t *testing.T) {
	for _, step := range []int{0, 2} {
		t.Run(fmt.Sprint(step), func(t *testing.T) {
			ctx := context.Background()
			store := new(MockConfigStore)
			t.Cleanup(func() { store.AssertExpectations(t) })
			plan := bareStepAPIPlan()
			plan.AutoPurchase = false
			plan.RampSchedule.CurrentStep = step
			older := time.Now().AddDate(0, 0, 1)
			plan.NextExecutionDate = &older
			first := time.Now().AddDate(0, 0, 10)
			store.On("GetPurchasePlan", ctx, plan.ID).Return(plan, nil)
			store.On("GetPlanAccounts", ctx, plan.ID).Return([]config.CloudAccount{{Provider: "azure"}}, nil)
			var rows []config.PurchaseExecution
			store.On("SavePurchaseExecution", ctx, mock.Anything).Run(func(a mock.Arguments) { rows = append(rows, *a.Get(1).(*config.PurchaseExecution)) }).Return(nil).Times(2)
			store.On("UpdatePurchasePlan", ctx, mock.Anything).Return(nil)
			h := &Handler{config: store}
			_, err := h.createPlannedPurchasesTx(ctx, nil, plan.ID, 2, first, nil, &Session{UserID: apiKeyAdminUserID})
			require.NoError(t, err)
			require.Len(t, rows, 2)
			assert.True(t, rows[0].ScheduledDate.Equal(first))
			assert.True(t, rows[1].ScheduledDate.Equal(first.AddDate(0, 0, 7)))
			assert.Equal(t, step+1, rows[0].StepNumber)
			assert.True(t, first.Equal(*plan.NextExecutionDate))
			plan.RampSchedule.CurrentStep++
			assert.True(t, plan.RampSchedule.GetNextPurchaseDate().Equal(first.AddDate(0, 0, 7)))
		})
	}
}

func TestRetryOfBarePlanStepIsRefusedAndCreatesNothing(t *testing.T) {
	creator := retryCallerID
	failed := &config.PurchaseExecution{
		ExecutionID: bareStepAPIExecID,
		PlanID:      bareStepAPIPlanID,
		StepNumber:  1,
		Status:      "failed",
		// A transient failure that is not one of the persistent-failure
		// hints, so the refusal under test is the only gate that can fire.
		Error:           "failed to send approval email: SES throttle exceeded",
		CreatedByUserID: &creator,
		Source:          common.PurchaseSourceWeb,
	}
	// purchasesFiredByRetry asserts that a refused retry persisted no successor.
	tokens, err := purchasesFiredByRetry(t, failed, sessionRetryReq())

	assert.Empty(t, tokens)
	require.Error(t, err, "retrying a plan step with no recommendations must be refused")
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a client error, got %v", err)
	assert.Equal(t, 409, ce.code)
	assert.Contains(t, ce.Error(), "no recommendations")
}
