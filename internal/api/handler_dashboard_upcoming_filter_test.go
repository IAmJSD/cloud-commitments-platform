package api

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #713: GET /api/dashboard/upcoming took no filter parameters, so the
// Home widget could not follow the topbar provider and account chips. It now
// accepts provider and account_ids (plus the legacy account_id) like
// /api/dashboard/summary, with the account filter intersected with the
// session's allowed_accounts.

const (
	upAcctProd  = "aaaaaaaa-0000-4000-8000-000000000001"
	upAcctStage = "aaaaaaaa-0000-4000-8000-000000000002"
	upAcctGCP   = "aaaaaaaa-0000-4000-8000-000000000003"

	upPlanProd  = "bbbbbbbb-0000-4000-8000-000000000001"
	upPlanStage = "bbbbbbbb-0000-4000-8000-000000000002"
	upPlanGCP   = "bbbbbbbb-0000-4000-8000-000000000003"
	upPlanMulti = "bbbbbbbb-0000-4000-8000-000000000004"
)

var upcomingFilterAccounts = []config.CloudAccount{
	{ID: upAcctProd, Name: "Production", Provider: "aws", ExternalID: "123456789012"},
	{ID: upAcctStage, Name: "Staging", Provider: "aws", ExternalID: "111111111111"},
	{ID: upAcctGCP, Name: "Analytics", Provider: "gcp", ExternalID: "example-gcp-project"},
}

func upcomingFilterAccount(id string) config.CloudAccount {
	for _, a := range upcomingFilterAccounts {
		if a.ID == id {
			return a
		}
	}
	return config.CloudAccount{ID: id}
}

// upcomingFilterStore wires one pending execution per plan, plus a fan-out
// child of the multi-account plan pinned to Staging:
//
//	exec-prod        aws plan, accounts [Production]
//	exec-stage       aws plan, accounts [Staging]
//	exec-gcp         gcp plan, accounts [Analytics]
//	exec-multi       aws plan-level parent, accounts [Production, Staging]
//	exec-multi-stage aws fan-out child, cloud_account_id = Staging
func upcomingFilterStore() *MockConfigStore {
	store := new(MockConfigStore)
	scheduled := time.Now().AddDate(0, 0, 7)
	plan := func(id, name, provider, service string) config.PurchasePlan {
		return config.PurchasePlan{
			ID: id, Name: name, Enabled: true,
			Services:     map[string]config.ServiceConfig{provider + "/" + service: {Provider: provider, Service: service}},
			RampSchedule: config.RampSchedule{TotalSteps: 3},
		}
	}
	plans := []config.PurchasePlan{
		plan(upPlanProd, "Prod EC2", "aws", "ec2"),
		plan(upPlanStage, "Stage RDS", "aws", "rds"),
		plan(upPlanGCP, "GCP compute", "gcp", "compute"),
		plan(upPlanMulti, "Multi EC2", "aws", "ec2"),
	}
	stage := upAcctStage
	pending := []config.PurchaseExecution{
		{ExecutionID: "exec-prod", PlanID: upPlanProd, Status: "pending", ScheduledDate: scheduled, StepNumber: 1},
		{ExecutionID: "exec-stage", PlanID: upPlanStage, Status: "pending", ScheduledDate: scheduled, StepNumber: 1},
		{ExecutionID: "exec-gcp", PlanID: upPlanGCP, Status: "pending", ScheduledDate: scheduled, StepNumber: 1},
		{ExecutionID: "exec-multi", PlanID: upPlanMulti, Status: "pending", ScheduledDate: scheduled, StepNumber: 1},
		{ExecutionID: "exec-multi-stage", PlanID: upPlanMulti, CloudAccountID: &stage, Status: "pending", ScheduledDate: scheduled, StepNumber: 1},
	}
	planAccounts := map[string][]config.CloudAccount{
		upPlanProd:  {upcomingFilterAccount(upAcctProd)},
		upPlanStage: {upcomingFilterAccount(upAcctStage)},
		upPlanGCP:   {upcomingFilterAccount(upAcctGCP)},
		upPlanMulti: {upcomingFilterAccount(upAcctProd), upcomingFilterAccount(upAcctStage)},
	}

	store.On("GetPendingExecutions", mock.Anything).Return(pending, nil)
	store.On("ListPurchasePlans", mock.Anything, config.PurchasePlanFilter{}).Return(plans, nil)
	store.GetPlanAccountsFn = func(_ context.Context, planID string) ([]config.CloudAccount, error) {
		return planAccounts[planID], nil
	}
	store.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
		a := upcomingFilterAccount(id)
		return &a, nil
	}
	store.ListCloudAccountsFn = func(context.Context, config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return append([]config.CloudAccount(nil), upcomingFilterAccounts...), nil
	}
	return store
}

// upcomingAsViewer calls getUpcomingPurchases as a session restricted to
// allowed (nil = admin, unrestricted) with the given query parameters.
func upcomingAsViewer(t *testing.T, allowed []string, params map[string]string) (*UpcomingPurchaseResponse, error) {
	t.Helper()
	ctx := context.Background()
	store := upcomingFilterStore()
	var mockAuth *MockAuthService
	var req *events.LambdaFunctionURLRequest
	if allowed == nil {
		mockAuth, req = adminDashboardReq(ctx)
	} else {
		mockAuth = new(MockAuthService)
		mockAuth.On("ValidateSession", ctx, "viewer-token").Return(&Session{UserID: "viewer-1"}, nil)
		mockAuth.On("HasPermissionAPI", ctx, "viewer-1", "view", "purchases").Return(true, nil)
		mockAuth.On("GetAllowedAccountsAPI", ctx, "viewer-1").Return(allowed, nil)
		req = &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer viewer-token"}}
	}
	req.QueryStringParameters = params
	handler := &Handler{auth: mockAuth, config: store}
	return handler.getUpcomingPurchases(ctx, req)
}

// upcomingIDsFor returns the sorted execution IDs upcomingAsViewer lists.
func upcomingIDsFor(t *testing.T, allowed []string, params map[string]string) []string {
	t.Helper()
	resp, err := upcomingAsViewer(t, allowed, params)
	require.NoError(t, err)
	return upcomingIDs(resp)
}

func upcomingIDs(resp *UpcomingPurchaseResponse) []string {
	ids := make([]string, 0, len(resp.Purchases))
	for _, p := range resp.Purchases {
		ids = append(ids, p.ExecutionID)
	}
	sort.Strings(ids)
	return ids
}

func TestGetUpcomingPurchases_NoFilterUnchanged(t *testing.T) {
	ids := upcomingIDsFor(t, nil, nil)
	assert.Equal(t, []string{"exec-gcp", "exec-multi", "exec-multi-stage", "exec-prod", "exec-stage"}, ids)

	ids = upcomingIDsFor(t, nil, map[string]string{"provider": "all", "account_ids": ""})
	assert.Equal(t, []string{"exec-gcp", "exec-multi", "exec-multi-stage", "exec-prod", "exec-stage"}, ids,
		"provider=all and an empty account_ids mean no filter")
}

func TestGetUpcomingPurchases_ProviderFilter(t *testing.T) {
	ids := upcomingIDsFor(t, nil, map[string]string{"provider": "gcp"})
	assert.Equal(t, []string{"exec-gcp"}, ids)

	ids = upcomingIDsFor(t, nil, map[string]string{"provider": "aws"})
	assert.Equal(t, []string{"exec-multi", "exec-multi-stage", "exec-prod", "exec-stage"}, ids)

	ids = upcomingIDsFor(t, nil, map[string]string{"provider": "azure"})
	assert.Empty(t, ids)
}

func TestGetUpcomingPurchases_InvalidFilterIs400(t *testing.T) {
	for name, params := range map[string]map[string]string{
		"unknown provider":     {"provider": "oracle"},
		"non-uuid account_ids": {"account_ids": "not-a-uuid"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := upcomingAsViewer(t, nil, params)
			require.Error(t, err)
			ce, ok := IsClientError(err)
			require.True(t, ok, "want a client error, got %v", err)
			assert.Equal(t, 400, ce.code)
		})
	}
}

func TestGetUpcomingPurchases_AccountFilter(t *testing.T) {
	// A plan-level parent buys in every plan account, so it matches when any
	// of them is selected; the fan-out child matches only its own account.
	ids := upcomingIDsFor(t, nil, map[string]string{"account_ids": upAcctProd})
	assert.Equal(t, []string{"exec-multi", "exec-prod"}, ids)

	ids = upcomingIDsFor(t, nil, map[string]string{"account_ids": upAcctStage + "," + upAcctGCP})
	assert.Equal(t, []string{"exec-gcp", "exec-multi", "exec-multi-stage", "exec-stage"}, ids)

	// Legacy singular account_id carrying the external account number.
	ids = upcomingIDsFor(t, nil, map[string]string{"account_id": "111111111111"})
	assert.Equal(t, []string{"exec-multi", "exec-multi-stage", "exec-stage"}, ids)

	// account_ids wins over account_id, as on the summary route.
	ids = upcomingIDsFor(t, nil, map[string]string{"account_ids": upAcctGCP, "account_id": upAcctProd})
	assert.Equal(t, []string{"exec-gcp"}, ids)

	// An account no plan targets matches nothing rather than everything.
	ids = upcomingIDsFor(t, nil, map[string]string{"account_ids": "cccccccc-0000-4000-8000-000000000009"})
	assert.Empty(t, ids)
}

func TestGetUpcomingPurchases_ProviderAndAccountCombine(t *testing.T) {
	ids := upcomingIDsFor(t, nil, map[string]string{"provider": "gcp", "account_ids": upAcctProd + "," + upAcctGCP})
	assert.Equal(t, []string{"exec-gcp"}, ids)

	ids = upcomingIDsFor(t, nil, map[string]string{"provider": "gcp", "account_ids": upAcctProd})
	assert.Empty(t, ids)
}

// A restricted session naming an account outside its allowed_accounts gets
// that account dropped from the filter (an empty result, not someone else's
// rows and not an error), the same as /api/dashboard/summary.
func TestGetUpcomingPurchases_OutOfScopeAccount(t *testing.T) {
	allowed := []string{upAcctProd, upAcctGCP}

	// Baseline: the session scope alone hides every row touching Staging.
	ids := upcomingIDsFor(t, allowed, nil)
	assert.Equal(t, []string{"exec-gcp", "exec-prod"}, ids)

	ids = upcomingIDsFor(t, allowed, map[string]string{"account_ids": upAcctStage})
	assert.Empty(t, ids, "an out-of-scope account must not widen or bypass the session scope")

	ids = upcomingIDsFor(t, allowed, map[string]string{"account_ids": upAcctStage + "," + upAcctProd})
	assert.Equal(t, []string{"exec-prod"}, ids, "the in-scope part of a mixed filter still applies")

	ids = upcomingIDsFor(t, allowed, map[string]string{"account_id": "111111111111"})
	assert.Empty(t, ids, "a legacy external number for an out-of-scope account matches nothing")

	// An in-scope legacy external number is re-grouped under its real
	// provider by the intersection and still matches.
	ids = upcomingIDsFor(t, allowed, map[string]string{"account_id": "123456789012"})
	assert.Equal(t, []string{"exec-prod"}, ids)

	ids = upcomingIDsFor(t, allowed, map[string]string{"account_ids": upAcctGCP})
	assert.Equal(t, []string{"exec-gcp"}, ids)
}

// Through the real router: the query string reaches the handler.
func TestHandleRequest_DashboardUpcomingAppliesQueryFilters(t *testing.T) {
	ctx := context.Background()
	handler := &Handler{config: upcomingFilterStore(), corsAllowedOrigin: "*", apiKey: "test-key"}
	req := &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"X-API-Key": "test-key"},
		QueryStringParameters: map[string]string{"provider": "aws", "account_ids": upAcctStage},
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "GET", Path: "/api/dashboard/upcoming"},
		},
	}

	resp, err := handler.HandleRequest(ctx, req)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode, resp.Body)

	var body UpcomingPurchaseResponse
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &body))
	assert.Equal(t, []string{"exec-multi", "exec-multi-stage", "exec-stage"}, upcomingIDs(&body))
}
