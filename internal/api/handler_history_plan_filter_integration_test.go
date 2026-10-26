//go:build integration

package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type planFilterEnv struct {
	*historySummaryEnv
	container     *testhelpers.PostgresContainer
	planA, planB  string
	admin, scoped *Handler
}

func (e *planFilterEnv) createPlan(ctx context.Context, t *testing.T, name string) string {
	t.Helper()
	next := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	plan := &config.PurchasePlan{
		Name: name, Enabled: true,
		Services:          map[string]config.ServiceConfig{"ec2": {Provider: "aws", Service: "ec2", Enabled: true}},
		RampSchedule:      config.RampSchedule{Type: "weekly", PercentPerStep: 25, StepIntervalDays: 7, TotalSteps: 4, StartDate: time.Now().AddDate(0, 0, -7)},
		NextExecutionDate: &next,
	}
	require.NoError(t, plan.Validate())
	require.NoError(t, e.store.CreatePurchasePlan(ctx, plan))
	return plan.ID
}

func (e *planFilterEnv) addPlanPurchase(ctx context.Context, t *testing.T, tenant, planID string, at time.Time) {
	t.Helper()
	acct := e.account(tenant)
	up := 10.0
	require.NoError(t, e.store.SavePurchaseHistory(ctx, &config.PurchaseHistoryRecord{
		AccountID: acct.ExternalID, PurchaseID: uuid.NewString(), Timestamp: at, Provider: "aws", Service: "ec2",
		Region: "us-east-1", ResourceType: "t3.micro", Count: 1, Term: 1, Payment: "no-upfront",
		UpfrontCost: &up, EstimatedSavings: 1, PlanID: planID, CloudAccountID: &acct.ID,
	}))
}

func (e *planFilterEnv) addPlanExecution(ctx context.Context, t *testing.T, tenant, planID, status, execErr string, at time.Time) {
	t.Helper()
	exec := &config.PurchaseExecution{
		PlanID: planID, ExecutionID: uuid.NewString(), IdempotencyKey: uuid.NewString(), Status: status, Error: execErr,
		ScheduledDate: at, TotalUpfrontCost: 500, EstimatedSavings: 50,
	}
	if tenant != "" {
		id := e.account(tenant).ID
		exec.CloudAccountID = &id
		exec.Recommendations = []config.RecommendationRecord{{Provider: "aws", Service: "ec2", Region: "us-east-1", CloudAccountID: &id}}
	}
	require.NoError(t, e.store.SavePurchaseExecution(ctx, exec))
}

// newPlanFilterEnv seeds plan A (tenant A) and plan B (tenant B), each with two
// purchases, a failed and a pending execution, plus tenant A rows tied to no
// plan. The pending rows are recent, so the stale-expiry sweep leaves them.
func newPlanFilterEnv(ctx context.Context, t *testing.T) *planFilterEnv {
	t.Helper()
	base, container := setupHistorySummaryEnv(ctx, t)
	e := &planFilterEnv{historySummaryEnv: base, container: container}
	e.planA, e.planB = e.createPlan(ctx, t, "plan-a"), e.createPlan(ctx, t, "plan-b")
	at := time.Now().UTC().Add(-3 * 24 * time.Hour).Truncate(time.Second)
	for i, p := range []struct{ tenant, plan string }{{"a", e.planA}, {"b", e.planB}, {"a", ""}} {
		o := time.Duration(i) * time.Minute
		e.addPlanPurchase(ctx, t, p.tenant, p.plan, at.Add(o))
		e.addPlanPurchase(ctx, t, p.tenant, p.plan, at.Add(o+time.Hour))
		e.addPlanExecution(ctx, t, p.tenant, p.plan, "failed", "boom", at.Add(o+2*time.Hour))
		e.addPlanExecution(ctx, t, p.tenant, p.plan, "pending", "", at.Add(o+3*time.Hour))
	}
	scopedAuth := new(MockAuthService)
	scopedAuth.On("ValidateSession", mock.Anything, scopedToken).Return(&Session{UserID: scopedUserID}, nil).Maybe()
	scopedAuth.On("GetUser", mock.Anything, mock.Anything).Return(&User{Email: "creator@example.com"}, nil).Maybe()
	scopedAuth.grantScoped(e.tenantA.Name)
	adminAuth := new(MockAuthService)
	adminAuth.On("GetUser", mock.Anything, mock.Anything).Return(&User{Email: "creator@example.com"}, nil).Maybe()
	adminAuth.On("ValidateSession", mock.Anything, "admin-token").Return(&Session{UserID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}, nil).Maybe()
	adminAuth.grantAdmin()
	e.admin = &Handler{config: e.store, auth: adminAuth}
	e.scoped = &Handler{config: e.store, auth: scopedAuth}
	return e
}

func historyBody(ctx context.Context, t *testing.T, h *Handler, token string, params map[string]string) (HistoryResponse, string) {
	t.Helper()
	resp, _ := getHistoryJSON(ctx, t, h, token, params)
	raw, err := json.Marshal(resp)
	require.NoError(t, err)
	return resp, string(raw)
}

func assertAllPlan(t *testing.T, resp HistoryResponse, plan string) {
	t.Helper()
	for i := range resp.Purchases {
		assert.Equal(t, plan, resp.Purchases[i].PlanID, "row %s", resp.Purchases[i].PurchaseID)
	}
}

// TestIntegration_HistoryPlanFilter_Canonicalised: an uppercase or padded
// plan_id selects the same purchases and executions as the lowercase one, so
// the SQL predicate and the in-memory execution check agree.
func TestIntegration_HistoryPlanFilter_Canonicalised(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	e := newPlanFilterEnv(ctx, t)

	resp, want := historyBody(ctx, t, e.admin, "admin-token", map[string]string{"plan_id": e.planA})
	require.Len(t, resp.Purchases, 4, "2 purchases + failed + pending execution of plan A")
	assertAllPlan(t, resp, e.planA)
	assert.Equal(t, 4, resp.Summary.TotalPurchases)
	for _, variant := range []string{strings.ToUpper(e.planA), "  " + e.planA + " "} {
		_, got := historyBody(ctx, t, e.admin, "admin-token", map[string]string{"plan_id": variant})
		assert.Equal(t, want, got, variant)
	}
	all, _ := historyBody(ctx, t, e.admin, "admin-token", map[string]string{})
	assert.Len(t, all.Purchases, 12, "no plan filter returns every plan and planless rows")
}

// TestIntegration_HistoryPlanFilter_CrossTenant: a user scoped to tenant A who
// passes tenant B's plan_id gets exactly what a random plan_id gets, and
// tenant A's own plan returns only tenant A's rows.
func TestIntegration_HistoryPlanFilter_CrossTenant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	e := newPlanFilterEnv(ctx, t)

	adminB, _ := historyBody(ctx, t, e.admin, "admin-token", map[string]string{"plan_id": e.planB})
	require.Len(t, adminB.Purchases, 4, "tenant B's plan is not empty for an unrestricted viewer")

	foreign, foreignBody := historyBody(ctx, t, e.scoped, scopedToken, map[string]string{"plan_id": e.planB})
	_, randomBody := historyBody(ctx, t, e.scoped, scopedToken, map[string]string{"plan_id": uuid.NewString()})
	assert.Empty(t, foreign.Purchases)
	assertSummaryEqual(t, HistorySummary{TotalUpfront: new(float64)}, foreign.Summary)
	assert.Equal(t, randomBody, foreignBody, "another tenant's plan must be indistinguishable from a missing one")

	own, _ := historyBody(ctx, t, e.scoped, scopedToken, map[string]string{"plan_id": e.planA})
	require.Len(t, own.Purchases, 4)
	assertAllPlan(t, own, e.planA)
	assert.Equal(t, 4, own.Summary.TotalPurchases)
}

// TestIntegration_HistoryPlanFilter_ActionableOverCap: more than MaxListLimit
// other-plan actionable executions, all newer, must not push the plan's own
// pending execution out of the listing.
func TestIntegration_HistoryPlanFilter_ActionableOverCap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	e := newPlanFilterEnv(ctx, t)
	_, err := e.container.DB.Pool().Exec(ctx, `
		INSERT INTO purchase_executions (plan_id, status, scheduled_date, idempotency_key)
		SELECT $1, 'approved', now() - interval '1 hour' - g * interval '1 second', gen_random_uuid()::text
		FROM generate_series(1, $2) g`, e.planB, config.MaxListLimit+5)
	require.NoError(t, err)

	resp, _ := historyBody(ctx, t, e.admin, "admin-token", map[string]string{"plan_id": e.planA})
	require.Len(t, resp.Purchases, 4)
	assertAllPlan(t, resp, e.planA)
	pending := 0
	for i := range resp.Purchases {
		if resp.Purchases[i].Status == "pending" {
			pending++
		}
	}
	assert.Equal(t, 1, pending, "the plan's pending execution survives the other plan's rows")
}
