//go:build integration

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func managedGuardRequest(body string) *events.LambdaFunctionURLRequest {
	return &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"Authorization": "Bearer admin-token", "Content-Type": "application/json"},
		Body:    body,
	}
}

func requireEditorRefusal(t *testing.T, err error) {
	t.Helper()
	var ce *clientError
	require.True(t, errors.As(err, &ce), "want a client error, got %v", err)
	require.Equal(t, http.StatusConflict, ce.code)
	require.Equal(t, codeEditorReconciliationRequired, ce.details["code"])
	require.Contains(t, ce.message, "editor")
	body, mErr := json.Marshal(ce.details)
	require.NoError(t, mErr)
	require.Contains(t, string(body), `"editor_reconciliation_required"`)
}

func setPlanManaged(ctx context.Context, t *testing.T, f *createConcurrencyFixture, managed bool) {
	t.Helper()
	markers := `{}`
	if managed {
		markers = `{"dates_managed":true,"allocation_mode":"equal_count"}`
	}
	_, err := f.pool.Exec(ctx, `UPDATE purchase_plans SET next_execution_date = now() + interval '3 days',
		ramp_schedule = (jsonb_set(ramp_schedule, '{current_step}', '0') - 'dates_managed') || $2::jsonb WHERE id = $1`, f.planID, markers)
	require.NoError(t, err)
}

func TestLegacyWritesOnManagedPlanMapToEditorRefusal(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		patch   bool
		body    string
		managed bool
		refused bool
	}{
		{"put sensitive ramp change", false, `{"name":"p","enabled":true,"notification_days_before":2,"ramp_schedule":"immediate"}`, true, true},
		{"put sensitive notice change", false, `{"name":"p","enabled":true,"notification_days_before":9,"ramp_schedule":"weekly-25pct"}`, true, true},
		{"patch notice", true, `{"notification_days_before":7}`, true, true},
		{"patch arming", true, `{"auto_purchase":true}`, true, true},
		{"patch name", true, `{"name":"renamed"}`, true, false},
		{"patch disable", true, `{"enabled":false}`, true, false},
		{"unmanaged put", false, `{"name":"p","enabled":true,"notification_days_before":2,"ramp_schedule":"immediate"}`, false, false},
		{"unmanaged patch notice", true, `{"notification_days_before":7}`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCreateConcurrencyFixture(ctx, t)
			setPlanManaged(ctx, t, f, tc.managed)
			var err error
			if tc.patch {
				_, err = f.handler.patchPlan(ctx, managedGuardRequest(tc.body), f.planID)
			} else {
				_, err = f.handler.updatePlan(ctx, managedGuardRequest(tc.body), f.planID)
			}
			if tc.refused {
				requireEditorRefusal(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestManagedPlanRefusalIsHTTP409WithCode(t *testing.T) {
	ctx := context.Background()
	f := newCreateConcurrencyFixture(ctx, t)
	setPlanManaged(ctx, t, f, true)
	f.handler.auth.(*MockAuthService).On("ValidateCSRFToken", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	req := managedGuardRequest(`{"notification_days_before":7}`)
	req.Headers["X-CSRF-Token"] = "test-csrf"
	req.RequestContext.HTTP.Method = "PATCH"
	req.RequestContext.HTTP.Path = "/api/plans/" + f.planID

	resp, err := f.handler.HandleRequest(ctx, req)
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, resp.StatusCode, resp.Body)
	require.JSONEq(t, `{"error":"this plan is managed by the schedule editor; make this change in the editor","code":"editor_reconciliation_required"}`, resp.Body)
}

// limitedHandler serves the same store to a caller holding only perms, with
// account scope restricted to scoped (nil means unrestricted).
func limitedHandler(f *createConcurrencyFixture, perms []auth.Permission, scoped []string) *Handler {
	limited := new(MockAuthService)
	limited.On("ValidateSession", mock.Anything, "admin-token").Return(&Session{UserID: "00000000-0000-0000-0000-000000000001", Email: "limited@example.com"}, nil).Maybe()
	limited.grantPermissionsScoped(perms, scoped)
	return &Handler{config: f.store, auth: limited}
}

func TestManagedPlanAuthorizationPrecedesGuard(t *testing.T) {
	ctx := context.Background()
	update := auth.Permission{Action: auth.ActionUpdate, Resource: auth.ResourcePlans}
	cases := []struct {
		name   string
		perms  []auth.Permission
		scoped []string
		patch  bool
		body   string
		status int
	}{
		{"no update:plans patch", []auth.Permission{{Action: auth.ActionView, Resource: auth.ResourcePlans}}, nil, true, `{"notification_days_before":7}`, http.StatusForbidden},
		{"no update:plans put", []auth.Permission{{Action: auth.ActionView, Resource: auth.ResourcePlans}}, nil, false, `{"name":"p","enabled":true,"notification_days_before":9,"ramp_schedule":"weekly-25pct"}`, http.StatusForbidden},
		{"out of scope account", []auth.Permission{{Action: auth.ActionAdmin, Resource: auth.ResourceAll}}, []string{"other-account"}, true, `{"notification_days_before":7}`, http.StatusNotFound},
		{"missing arm grant", []auth.Permission{update}, nil, true, `{"auto_purchase":true}`, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCreateConcurrencyFixture(ctx, t)
			setPlanManaged(ctx, t, f, true)
			_, err := f.pool.Exec(ctx, `UPDATE purchase_plans SET max_step_commitment = 1000 WHERE id = $1`, f.planID)
			require.NoError(t, err)
			h := limitedHandler(f, tc.perms, tc.scoped)
			if tc.patch {
				_, err = h.patchPlan(ctx, managedGuardRequest(tc.body), f.planID)
			} else {
				_, err = h.updatePlan(ctx, managedGuardRequest(tc.body), f.planID)
			}
			var ce *clientError
			if errors.As(err, &ce) {
				require.Equal(t, tc.status, ce.code, ce.message)
			} else {
				require.True(t, IsNotFoundError(err) && tc.status == http.StatusNotFound, "got %v", err)
			}
		})
	}
}

func TestDisablePlanSucceedsOnManagedPlan(t *testing.T) {
	ctx := context.Background()
	f := newCreateConcurrencyFixture(ctx, t)
	setPlanManaged(ctx, t, f, true)
	require.NoError(t, f.handler.disablePlan(ctx, f.planID))
	var enabled bool
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT enabled FROM purchase_plans WHERE id = $1`, f.planID).Scan(&enabled))
	require.False(t, enabled)
}
