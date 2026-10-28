package api

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlanRequest_toPurchasePlan(t *testing.T) {
	t.Run("basic conversion with defaults", func(t *testing.T) {
		req := &PlanRequest{
			Name:         "Test Plan",
			Enabled:      true,
			AutoPurchase: false,
		}

		plan := req.toPurchasePlan()

		assert.Equal(t, "Test Plan", plan.Name)
		assert.True(t, plan.Enabled)
		assert.False(t, plan.AutoPurchase)
		// Default ramp schedule should be immediate
		assert.Equal(t, "immediate", plan.RampSchedule.Type)
		assert.NotNil(t, plan.NextExecutionDate)
	})

	t.Run("with provider and service", func(t *testing.T) {
		req := &PlanRequest{
			Name:     "RDS Plan",
			Enabled:  true,
			Provider: "aws",
			Service:  "rds",
		}

		plan := req.toPurchasePlan()

		require.NotNil(t, plan.Services)
		assert.Len(t, plan.Services, 1)

		svc, exists := plan.Services["aws/rds"]
		require.True(t, exists)
		assert.Equal(t, "aws", svc.Provider)
		assert.Equal(t, "rds", svc.Service)
		assert.True(t, svc.Enabled)
		// Defaults
		assert.Equal(t, 3, svc.Term)
		assert.Equal(t, "no-upfront", svc.Payment)
		assert.Equal(t, 80.0, svc.Coverage)
	})

	t.Run("with custom term and payment", func(t *testing.T) {
		req := &PlanRequest{
			Name:           "Custom Plan",
			Enabled:        true,
			Provider:       "aws",
			Service:        "ec2",
			Term:           1,
			Payment:        "all-upfront",
			TargetCoverage: ptr(90),
		}

		plan := req.toPurchasePlan()

		svc := plan.Services["aws/ec2"]
		assert.Equal(t, 1, svc.Term)
		assert.Equal(t, "all-upfront", svc.Payment)
		assert.Equal(t, 90.0, svc.Coverage)
	})

	t.Run("with weekly ramp schedule preset", func(t *testing.T) {
		req := &PlanRequest{
			Name:         "Weekly Plan",
			Enabled:      true,
			RampSchedule: "weekly-25pct",
		}

		plan := req.toPurchasePlan()

		assert.Equal(t, "weekly", plan.RampSchedule.Type)
		assert.Equal(t, 25.0, plan.RampSchedule.PercentPerStep)
		assert.Equal(t, 7, plan.RampSchedule.StepIntervalDays)
		assert.Equal(t, 4, plan.RampSchedule.TotalSteps)
	})

	t.Run("with monthly ramp schedule preset", func(t *testing.T) {
		req := &PlanRequest{
			Name:         "Monthly Plan",
			Enabled:      true,
			RampSchedule: "monthly-10pct",
		}

		plan := req.toPurchasePlan()

		assert.Equal(t, "monthly", plan.RampSchedule.Type)
		assert.Equal(t, 10.0, plan.RampSchedule.PercentPerStep)
		assert.Equal(t, 30, plan.RampSchedule.StepIntervalDays)
		assert.Equal(t, 10, plan.RampSchedule.TotalSteps)
	})

	t.Run("with custom ramp schedule", func(t *testing.T) {
		req := &PlanRequest{
			Name:               "Custom Ramp Plan",
			Enabled:            true,
			RampSchedule:       "custom",
			CustomStepPercent:  50,
			CustomIntervalDays: 14,
		}

		plan := req.toPurchasePlan()

		assert.Equal(t, "custom", plan.RampSchedule.Type)
		assert.Equal(t, 50.0, plan.RampSchedule.PercentPerStep)
		assert.Equal(t, 14, plan.RampSchedule.StepIntervalDays)
		assert.Equal(t, 2, plan.RampSchedule.TotalSteps) // 100/50 = 2
	})

	t.Run("custom ramp schedule with defaults for invalid values", func(t *testing.T) {
		req := &PlanRequest{
			Name:               "Custom Plan Bad Values",
			Enabled:            true,
			RampSchedule:       "custom",
			CustomStepPercent:  0,  // Invalid, should default
			CustomIntervalDays: -5, // Invalid, should default
		}

		plan := req.toPurchasePlan()

		assert.Equal(t, "custom", plan.RampSchedule.Type)
		assert.Equal(t, 20.0, plan.RampSchedule.PercentPerStep) // Default
		assert.Equal(t, 7, plan.RampSchedule.StepIntervalDays)  // Default
		assert.Equal(t, 5, plan.RampSchedule.TotalSteps)        // 100/20 = 5
	})

	t.Run("unknown ramp schedule defaults to immediate", func(t *testing.T) {
		req := &PlanRequest{
			Name:         "Unknown Ramp Plan",
			Enabled:      true,
			RampSchedule: "unknown-schedule",
		}

		plan := req.toPurchasePlan()

		assert.Equal(t, "immediate", plan.RampSchedule.Type)
	})

	t.Run("timestamps are set", func(t *testing.T) {
		req := &PlanRequest{
			Name:    "Timestamped Plan",
			Enabled: true,
		}

		plan := req.toPurchasePlan()

		assert.False(t, plan.CreatedAt.IsZero())
		assert.False(t, plan.UpdatedAt.IsZero())
		assert.False(t, plan.RampSchedule.StartDate.IsZero())
	})

	t.Run("notification days before is set", func(t *testing.T) {
		req := &PlanRequest{
			Name:                   "Notification Plan",
			Enabled:                true,
			NotificationDaysBefore: 5,
		}

		plan := req.toPurchasePlan()

		assert.Equal(t, 5, plan.NotificationDaysBefore)
	})

	t.Run("no services when provider or service empty", func(t *testing.T) {
		tests := []struct {
			name     string
			provider string
			service  string
		}{
			{"empty provider", "", "rds"},
			{"empty service", "aws", ""},
			{"both empty", "", ""},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				req := &PlanRequest{
					Name:     "Test Plan",
					Enabled:  true,
					Provider: tt.provider,
					Service:  tt.service,
				}

				plan := req.toPurchasePlan()

				assert.Nil(t, plan.Services)
			})
		}
	})
}

func planFromBody(t *testing.T, body string) *PlanRequest {
	t.Helper()
	var req PlanRequest
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	return &req
}

func TestPlanRequest_TargetCoverageDefault(t *testing.T) {
	const base = `{"name":"p","provider":"aws","service":"ec2"`
	tests := []struct {
		name string
		body string
		want float64
	}{
		{"explicit 1 is kept", base + `,"target_coverage":1}`, 1},
		{"omitted defaults to 80", base + `}`, 80},
		{"explicit value is kept", base + `,"target_coverage":55}`, 55},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := planFromBody(t, tt.body).toPurchasePlan()
			assert.Equal(t, tt.want, plan.Services["aws/ec2"].Coverage)
		})
	}
}

func TestPlanRequest_validateTargetCoverage(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"omitted is allowed", `{"name":"p"}`, false},
		{"1 is allowed", `{"target_coverage":1}`, false},
		{"100 is allowed", `{"target_coverage":100}`, false},
		{"0 is refused", `{"target_coverage":0}`, true},
		{"-1 is refused", `{"target_coverage":-1}`, true},
		{"101 is refused", `{"target_coverage":101}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := planFromBody(t, tt.body).validateTargetCoverage()
			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			ce, ok := IsClientError(err)
			require.True(t, ok)
			assert.Equal(t, 400, ce.code)
			assert.Equal(t, "target_coverage must be between 1 and 100 for a purchase plan", ce.message)
		})
	}
}
