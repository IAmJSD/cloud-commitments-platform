//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func b1aRoute(t *testing.T, h *Handler, method, path, body string) events.LambdaFunctionURLResponse {
	t.Helper()
	h.auth.(*MockAuthService).On("ValidateCSRFToken", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	response, err := h.HandleRequest(context.Background(), &events.LambdaFunctionURLRequest{
		Headers:        map[string]string{"Authorization": "Bearer admin-token", "Content-Type": "application/json", "X-CSRF-Token": "test-csrf"},
		Body:           body,
		RequestContext: events.LambdaFunctionURLRequestContext{HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: method, Path: path}},
	})
	require.NoError(t, err)
	require.NotNil(t, response)
	return *response
}

func b1aFixture(t *testing.T) *createConcurrencyFixture {
	t.Helper()
	ctx := context.Background()
	f := newCreateConcurrencyFixture(ctx, t)
	plan, err := f.store.GetPurchasePlan(ctx, f.planID)
	require.NoError(t, err)
	next := time.Now().AddDate(0, 0, 7).Truncate(time.Microsecond)
	plan.NextExecutionDate = &next
	require.NoError(t, f.store.UpdatePurchasePlan(ctx, plan))
	return f
}

func b1aRow(t *testing.T, f *createConcurrencyFixture) string {
	t.Helper()
	var row string
	require.NoError(t, f.pool.QueryRow(context.Background(), "SELECT to_jsonb(p)::text FROM purchase_plans p WHERE id=$1", f.planID).Scan(&row))
	return row
}

func b1aRead(t *testing.T, f *createConcurrencyFixture) *config.PurchasePlan {
	t.Helper()
	stored, err := f.store.GetPurchasePlan(context.Background(), f.planID)
	require.NoError(t, err)
	response := b1aRoute(t, f.handler, "GET", "/api/plans/"+f.planID, "")
	require.Equal(t, 200, response.StatusCode, response.Body)
	var returned config.PurchasePlan
	require.NoError(t, json.Unmarshal([]byte(response.Body), &returned))
	want, err := json.Marshal(stored)
	require.NoError(t, err)
	got, err := json.Marshal(returned)
	require.NoError(t, err)
	require.JSONEq(t, string(want), string(got))
	return stored
}

const b1aUpdate = `{"name":"renamed","enabled":true,"notification_days_before":2,"ramp_schedule":"weekly-25pct"%s}`

func TestLegacyVersionB1aRoutedVersion(t *testing.T) {
	ctx := context.Background()
	f := b1aFixture(t)
	precision := time.Date(2026, 10, 1, 12, 0, 0, 123456000, time.UTC)
	cloneID := uuid.NewString()
	_, err := f.pool.Exec(ctx, `INSERT INTO purchase_plans
		(id, name, enabled, auto_purchase, notification_days_before, services, ramp_schedule, max_step_commitment, next_execution_date, last_execution_date, last_notification_sent, created_at, updated_at)
		SELECT $2, name, enabled, auto_purchase, notification_days_before, services, ramp_schedule, max_step_commitment, next_execution_date, last_execution_date, last_notification_sent, created_at, $3
		FROM purchase_plans WHERE id=$1`, f.planID, cloneID, precision)
	require.NoError(t, err)
	accounts, err := f.store.GetPlanAccounts(ctx, f.planID)
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	f.planID = cloneID
	require.NoError(t, f.store.SetPlanAccounts(ctx, f.planID, []string{accounts[0].ID}))
	initial := b1aRead(t, f)
	require.True(t, precision.Equal(initial.UpdatedAt))
	require.Equal(t, "2026-10-01T12:00:00.123456Z", initial.UpdatedAt.UTC().Format(time.RFC3339Nano))
	t.Logf("persisted precision token=%s", initial.UpdatedAt.UTC().Format(time.RFC3339Nano))
	for _, tc := range []struct {
		name, value string
		status      int
	}{
		{"exact persisted microseconds", "exact", 200},
		{"same instant alternate offset", "offset", 200},
		{"omitted", "", 200},
		{"null", "null", 200},
		{"empty", `""`, 200},
		{"malformed", `"yesterday"`, 400},
		{"numeric", "123", 400},
		{"boolean", "true", 400},
		{"array", "[]", 400},
		{"object", "{}", 400},
		{"stale before handler", `"2000-01-01T00:00:00Z"`, 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := b1aRead(t, f)
			value := tc.value
			if tc.name == "stale before handler" {
				value = fmt.Sprintf("%q", before.UpdatedAt.Format(time.RFC3339Nano))
				before.Name = "winning write"
				require.NoError(t, f.store.UpdatePurchasePlan(ctx, before))
				before = b1aRead(t, f)
			}
			row := b1aRow(t, f)
			require.Zero(t, before.UpdatedAt.Nanosecond()%1000)
			if value == "exact" {
				value = fmt.Sprintf("%q", before.UpdatedAt.Format(time.RFC3339Nano))
			}
			if value == "offset" {
				value = fmt.Sprintf("%q", before.UpdatedAt.In(time.FixedZone("fixture", 2*60*60)).Format(time.RFC3339Nano))
			}
			field := ""
			if value != "" {
				field = `,"expected_updated_at":` + value
			}
			response := b1aRoute(t, f.handler, "PUT", "/api/plans/"+f.planID, fmt.Sprintf(b1aUpdate, field))
			require.Equal(t, tc.status, response.StatusCode, response.Body)
			if tc.status == 400 {
				if tc.name == "malformed" {
					require.Contains(t, response.Body, "expected_updated_at must be an RFC3339 timestamp")
				} else {
					require.Contains(t, response.Body, "invalid request body")
				}
			}
			after := b1aRead(t, f)
			if tc.status != 200 {
				require.Equal(t, row, b1aRow(t, f), "refusal preserves full persisted row")
				if tc.status == 409 {
					require.Contains(t, response.Body, "plan changed; refresh")
				}
				return
			}
			before.Name, before.UpdatedAt = "renamed", after.UpdatedAt
			require.Equal(t, before, after)
		})
	}
	row := b1aRow(t, f)
	response := b1aRoute(t, f.handler, "PUT", "/api/plans/"+f.planID, "{")
	require.Equal(t, 400, response.StatusCode, response.Body)
	require.Contains(t, response.Body, "invalid request body")
	require.Equal(t, row, b1aRow(t, f))
}

func TestLegacyVersionB1aRoutedCreateCompatibility(t *testing.T) {
	f := b1aFixture(t)
	accounts, err := f.store.GetPlanAccounts(context.Background(), f.planID)
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	var expected *config.PurchasePlan
	for _, tc := range []struct{ name, value string }{
		{"omitted", ""}, {"null", "null"}, {"empty", `""`},
		{"malformed timestamp string ignored", `"yesterday"`},
		{"valid timestamp string ignored", `"2000-01-01T00:00:00Z"`},
		{"numeric value ignored", "123"}, {"boolean", "true"}, {"array", "[]"}, {"object", "{}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			field := ""
			if tc.value != "" {
				field = `,"expected_updated_at":` + tc.value
			}
			body := fmt.Sprintf(`{"name":"created","provider":"azure","service":"compute","term":1,"payment":"monthly","target_coverage":80,"ramp_schedule":"immediate","enabled":false,"target_accounts":[%q]%s}`, accounts[0].ID, field)
			response := b1aRoute(t, f.handler, "POST", "/api/plans", body)
			require.Equal(t, 200, response.StatusCode, response.Body)
			var created config.PurchasePlan
			require.NoError(t, json.Unmarshal([]byte(response.Body), &created))
			copyFixture := *f
			copyFixture.planID = created.ID
			stored := b1aRead(t, &copyFixture)
			assigned, err := f.store.GetPlanAccounts(context.Background(), created.ID)
			require.NoError(t, err)
			require.Equal(t, accounts, assigned, "every created plan retains account association")
			stored.ID = ""
			stored.CreatedAt, stored.UpdatedAt, stored.RampSchedule.StartDate = time.Time{}, time.Time{}, time.Time{}
			if stored.NextExecutionDate != nil {
				zero := time.Time{}
				stored.NextExecutionDate = &zero
			}
			if expected == nil {
				expected = stored
			}
			require.Equal(t, expected, stored, "unknown version does not affect persisted plan")
		})
	}
	response := b1aRoute(t, f.handler, "POST", "/api/plans", "{")
	require.Equal(t, 400, response.StatusCode, response.Body)
	require.Contains(t, response.Body, "invalid request body")
}

func TestLegacyVersionB1aRoutedConcurrentCompletion(t *testing.T) {
	ctx := context.Background()
	f := b1aFixture(t)
	before := b1aRead(t, f)
	require.NoError(t, f.store.SavePurchaseExecution(ctx, &config.PurchaseExecution{
		ExecutionID: uuid.NewString(), PlanID: f.planID, Status: config.StatusCompleted, StepNumber: 3, ScheduledDate: time.Now(),
	}))
	var winner string
	f.handler.config = &planEditReadBarrier{PostgresStore: f.store, afterRead: func() {
		require.NoError(t, f.store.CompletePlanStep(ctx, f.planID, 3))
		winner = b1aRow(t, f)
	}}
	field := fmt.Sprintf(`,"expected_updated_at":%q`, before.UpdatedAt.Format(time.RFC3339Nano))
	response := b1aRoute(t, f.handler, "PUT", "/api/plans/"+f.planID, fmt.Sprintf(b1aUpdate, field))
	require.Equal(t, 409, response.StatusCode, response.Body)
	require.Contains(t, response.Body, "plan changed; refresh")
	require.NotEmpty(t, winner)
	require.Equal(t, winner, b1aRow(t, f))
	require.Equal(t, 3, b1aRead(t, f).RampSchedule.CurrentStep)
}

func TestLegacyVersionB1aRoutedAuthorization(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		perms  []auth.Permission
		scope  []string
		field  string
		status int
	}{
		{"permission precedes stale", []auth.Permission{{Action: auth.ActionView, Resource: auth.ResourcePlans}}, nil, `,"expected_updated_at":"2000-01-01T00:00:00Z"`, 403},
		{"mixed account scope precedes stale", []auth.Permission{{Action: auth.ActionUpdate, Resource: auth.ResourcePlans}}, []string{}, `,"expected_updated_at":"2000-01-01T00:00:00Z"`, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := b1aFixture(t)
			h := f.handler
			if tc.perms != nil {
				h = limitedHandler(f, tc.perms, tc.scope)
			}
			if tc.scope != nil {
				accounts, err := f.store.GetPlanAccounts(ctx, f.planID)
				require.NoError(t, err)
				other := &config.CloudAccount{Name: "other", Provider: "azure", ExternalID: "fixture-other", Enabled: true}
				require.NoError(t, f.store.CreateCloudAccount(ctx, other))
				require.NoError(t, f.store.SetPlanAccounts(ctx, f.planID, []string{accounts[0].ID, other.ID}))
				h = limitedHandler(f, tc.perms, []string{accounts[0].ID})
			}
			row := b1aRow(t, f)
			response := b1aRoute(t, h, "PUT", "/api/plans/"+f.planID, fmt.Sprintf(b1aUpdate, tc.field))
			require.Equal(t, tc.status, response.StatusCode, response.Body)
			require.Equal(t, row, b1aRow(t, f))
			b1aRead(t, f)
		})
	}
}
