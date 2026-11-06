package purchase

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/email"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixture rows: nothing attaches recommendations to a plan step yet.
func TestBuildNotificationData_ShowsTotalCommitmentAndCap(t *testing.T) {
	monthly := 100.0
	capAmount := 5000.0
	plan := config.PurchasePlan{ID: "plan-1", Name: "Azure ramp", MaxStepCommitment: &capAmount}
	exec := &config.PurchaseExecution{
		ExecutionID: "exec-1", PlanID: "plan-1", ScheduledDate: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC),
		TotalUpfrontCost: 300,
		Recommendations: []config.RecommendationRecord{
			// Azure monthly billing: no upfront, the commitment is all recurring.
			{Service: "compute", ResourceType: "Standard_D2s_v3", Region: "westeurope", Count: 2, Term: 1, Payment: "monthly", MonthlyCost: &monthly},
			{Service: "ec2", ResourceType: "m5.large", Region: "us-east-1", AvailabilityZone: "us-east-1a", Count: 3, Term: 3, Payment: "partial-upfront", UpfrontCost: 300,
				Details: json.RawMessage(`{"platform":"Linux/UNIX","tenancy":"default"}`)},
		},
	}
	m := &Manager{}

	data := m.buildNotificationData(plan, exec, "tok", 3, "ops@example.invalid", true)

	assert.InDelta(t, 1500.0, data.TotalCommitment, 1e-9)
	assert.InDelta(t, 1200.0, data.TotalRecurring, 1e-9)
	assert.Equal(t, 5, data.TotalUnits)
	assert.InDelta(t, 5000.0, data.StepCap, 1e-9)
	require.Len(t, data.Recommendations, 2)
	assert.InDelta(t, 1200.0, data.Recommendations[0].Commitment, 1e-9, "an Azure monthly row is not $0")
	assert.Equal(t, "us-east-1a", data.Recommendations[1].AvailabilityZone)
	assert.Equal(t, "Linux/UNIX", data.Recommendations[1].Platform)
	assert.Equal(t, "default", data.Recommendations[1].Tenancy)

	body, err := email.RenderScheduledPurchaseEmail(data)
	require.NoError(t, err)
	assert.Contains(t, body, "Will buy 2 recommendation(s) (5 units) for $1500.00 total commitment (upfront $300.00 + $1200.00 recurring over the terms), cap $5000.00, due on November 3, 2026. It is bought once approved. Review & Edit below and approve it in the approval queue.")
	assert.NotContains(t, body, "unless canceled")

	// Auto plan with 4-eyes off: the original wording is unchanged.
	autoData := m.buildNotificationData(plan, exec, "tok", 3, "ops@example.invalid", false)
	autoBody, err := email.RenderScheduledPurchaseEmail(autoData)
	require.NoError(t, err)
	assert.Contains(t, autoBody, "cap $5000.00, on November 3, 2026 unless canceled.")
	assert.Contains(t, body, "Term: 1 yr monthly | Upfront $0.00 + $100.00/month | Commitment $1200.00")
	assert.Contains(t, body, "in us-east-1 (us-east-1a) | Linux/UNIX | default")
}

// #1771: a step that waits for a human approval (manual plan, or an auto plan
// while 4-eyes is on) must not tell the approver it buys unless canceled.
func TestPlanNeedsApproval_Modes(t *testing.T) {
	on := &config.GlobalConfig{RequireDifferentApprover: true}
	off := &config.GlobalConfig{}
	cases := []struct {
		name   string
		plan   config.PurchasePlan
		global *config.GlobalConfig
		want   bool
	}{
		{"manual plan", config.PurchasePlan{}, off, true},
		{"auto plan, 4-eyes on", config.PurchasePlan{AutoPurchase: true}, on, true},
		{"auto plan, 4-eyes off", config.PurchasePlan{AutoPurchase: true}, off, false},
		{"auto plan, nil config fails closed", config.PurchasePlan{AutoPurchase: true}, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, planNeedsApproval(tc.plan, tc.global))
		})
	}
}

func TestBuildNotificationData_ApprovalWordingByMode(t *testing.T) {
	capAmount := 5000.0
	plan := config.PurchasePlan{ID: "plan-1", Name: "ramp", MaxStepCommitment: &capAmount}
	exec := &config.PurchaseExecution{
		ExecutionID: "exec-1", PlanID: "plan-1", ScheduledDate: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC),
		Recommendations: []config.RecommendationRecord{
			{Service: "ec2", ResourceType: "m5.large", Region: "us-east-1", Count: 1, Term: 1, Payment: "all-upfront", UpfrontCost: 100},
		},
	}
	m := &Manager{}
	const totals = "cap $5000.00, "
	for _, tc := range []struct {
		name          string
		needsApproval bool
	}{
		{"manual plan or auto with 4-eyes on", true},
		{"auto plan with 4-eyes off", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := m.buildNotificationData(plan, exec, "tok", 3, "ops@example.invalid", tc.needsApproval)
			assert.Equal(t, tc.needsApproval, data.NeedsApproval)
			body, err := email.RenderScheduledPurchaseEmail(data)
			require.NoError(t, err)
			assert.Contains(t, body, totals, "the totals and cap sentence stays in both modes")
			if tc.needsApproval {
				assert.Contains(t, body, totals+"due on November 3, 2026. It is bought once approved. Review & Edit below and approve it in the approval queue.")
				assert.NotContains(t, body, "unless canceled")
				assert.NotContains(t, body, "before automatic execution")
			} else {
				assert.Contains(t, body, totals+"on November 3, 2026 unless canceled.")
				assert.Contains(t, body, "You have 3 days to modify or cancel before automatic execution.")
				assert.NotContains(t, body, "approval queue")
			}
		})
	}
}
