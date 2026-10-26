package api

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

const (
	actionableDevUUID   = "11111111-1111-4111-8111-111111111111"
	actionableOtherUUID = "22222222-2222-4222-8222-222222222222"
)

var actionableTestAccounts = []config.CloudAccount{
	{ID: actionableDevUUID, Name: "dev", Provider: "aws", ExternalID: "111111111111"},
	{ID: actionableOtherUUID, Name: "other", Provider: "aws", ExternalID: "222222222222"},
}

// actionableHistoryRequest drives GET /api/history through HandleRequest as
// user u-1 with the given allow-list and query string.
func actionableHistoryRequest(t *testing.T, store *MockConfigStore, scope []string, params map[string]string) HistoryResponse {
	t.Helper()
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, "tok").Return(&Session{UserID: "u-1", Email: "u@example.com"}, nil)
	mockAuth.On("HasPermissionAPI", mock.Anything, "u-1", mock.Anything, mock.Anything).Return(true, nil)
	mockAuth.On("GetAllowedAccountsAPI", mock.Anything, "u-1").Return(scope, nil)
	store.On("ListPurchaseHistoryPage", mock.Anything, mock.Anything, mock.Anything).Return([]config.PurchaseHistoryRecord{}, nil)
	store.On("ListHistoryExecutionsPage", mock.Anything, mock.Anything).Return([]config.PurchaseExecution{}, nil)
	store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{}, nil).Maybe()
	store.ListCloudAccountsFn = func(context.Context, config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return actionableTestAccounts, nil
	}

	h := &Handler{auth: mockAuth, config: store}
	resp, err := h.HandleRequest(ctx, &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"Authorization": "Bearer tok"},
		QueryStringParameters: params,
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "GET", Path: "/api/history"},
		},
	})
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode, "body: %s", resp.Body)
	var body HistoryResponse
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &body))
	return body
}

func isActionableQuery(q config.ExecutionsByStatusesQuery) bool {
	return slices.Equal(q.Statuses, actionableExecutionStatuses)
}

func isStaleSweepQuery(q config.ExecutionsByStatusesQuery) bool {
	return slices.Equal(q.Statuses, []string{"pending", "notified"})
}

func pendingForAccount(id, account string, scheduled time.Time) config.PurchaseExecution {
	return config.PurchaseExecution{
		ExecutionID: id, Status: "pending", ScheduledDate: scheduled,
		CloudAccountID:  &account,
		Recommendations: []config.RecommendationRecord{{Provider: "aws", Service: "ec2", CloudAccountID: &account, Count: 1}},
	}
}

// Issue #632: a scoped user's pending approval must not be hidden because
// newer actionable rows of accounts outside the scope fill the class cap. The
// unfiltered read returns MaxListLimit of those rows; the scoped read the
// handler now issues returns the user's own row.
func TestHandleRequest_History_ScopedPendingNotCrowdedOutByOtherAccounts(t *testing.T) {
	now := time.Now()
	others := make([]config.PurchaseExecution, config.MaxListLimit)
	for i := range others {
		others[i] = pendingForAccount("other", actionableOtherUUID, now)
	}
	mine := pendingForAccount("mine", actionableDevUUID, now.Add(-time.Hour))

	store := new(MockConfigStore)
	store.On("GetExecutionsByStatuses", mock.Anything, mock.Anything, mock.Anything).Return(others, nil)
	store.On("GetExecutionsByStatusesFiltered", mock.Anything, mock.MatchedBy(func(q config.ExecutionsByStatusesQuery) bool {
		return isActionableQuery(q) && q.Scope != nil && slices.Contains(q.Scope.AccountIDs, actionableDevUUID) &&
			!slices.Contains(q.Scope.AccountIDs, actionableOtherUUID)
	})).Return([]config.PurchaseExecution{mine}, nil)
	store.On("GetExecutionsByStatusesFiltered", mock.Anything, mock.MatchedBy(isStaleSweepQuery)).Return([]config.PurchaseExecution{}, nil)

	body := actionableHistoryRequest(t, store, []string{"dev"}, map[string]string{})

	ids := make([]string, 0, len(body.Purchases))
	for _, p := range body.Purchases {
		ids = append(ids, p.PurchaseID)
	}
	assert.Equal(t, []string{"mine"}, ids)
	assert.False(t, body.Truncated, "the caller's class is not capped")
}

// The request's provider and date filters reach the actionable SQL read, and
// an unrestricted caller sends no scope.
func TestHandleRequest_History_ActionableQueryCarriesFilters(t *testing.T) {
	var got config.ExecutionsByStatusesQuery
	store := new(MockConfigStore)
	store.On("GetExecutionsByStatusesFiltered", mock.Anything, mock.MatchedBy(isActionableQuery)).
		Run(func(args mock.Arguments) { got = args.Get(1).(config.ExecutionsByStatusesQuery) }).
		Return([]config.PurchaseExecution{}, nil).Once()
	store.On("GetExecutionsByStatusesFiltered", mock.Anything, mock.MatchedBy(isStaleSweepQuery)).Return([]config.PurchaseExecution{}, nil).Once()

	actionableHistoryRequest(t, store, []string{"*"}, map[string]string{"provider": "aws", "start": "2026-01-01", "end": "2026-01-31"})

	assert.Equal(t, "aws", got.Provider)
	assert.Nil(t, got.Scope, "an unrestricted caller is not scoped in SQL")
	assert.Equal(t, config.MaxListLimit, got.Limit)
	require.NotNil(t, got.Start)
	require.NotNil(t, got.End)
	assert.Equal(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), *got.Start)
	assert.Equal(t, time.Date(2026, 1, 31, 23, 59, 59, 0, time.UTC), *got.End)
	assert.Empty(t, got.AccountIDs)
	store.AssertExpectations(t)
}

// A narrowed actionable read must not narrow the stale-approval sweep: the
// History load is what expires stale approvals, whoever loads it. The sweep
// reads pending/notified rows past the expiry window with no filter or scope.
// An unfiltered, unrestricted load reuses the class it already read.
func TestHandleRequest_History_StaleSweepIgnoresFiltersAndScope(t *testing.T) {
	t.Run("narrowed", func(t *testing.T) {
		var sweep config.ExecutionsByStatusesQuery
		store := new(MockConfigStore)
		store.On("GetExecutionsByStatusesFiltered", mock.Anything, mock.MatchedBy(isActionableQuery)).Return([]config.PurchaseExecution{}, nil)
		store.On("GetExecutionsByStatusesFiltered", mock.Anything, mock.MatchedBy(isStaleSweepQuery)).
			Run(func(args mock.Arguments) { sweep = args.Get(1).(config.ExecutionsByStatusesQuery) }).
			Return([]config.PurchaseExecution{}, nil).Once()

		before := time.Now()
		actionableHistoryRequest(t, store, []string{"dev"}, map[string]string{"provider": "azure"})

		store.AssertExpectations(t)
		assert.Nil(t, sweep.Scope)
		assert.Empty(t, sweep.Provider)
		assert.Empty(t, sweep.AccountIDs)
		assert.Nil(t, sweep.Start)
		require.NotNil(t, sweep.End)
		assert.WithinDuration(t, before.Add(-approvalExpiryWindow), *sweep.End, time.Minute)
	})
	t.Run("not narrowed", func(t *testing.T) {
		store := new(MockConfigStore)
		store.On("GetExecutionsByStatuses", mock.Anything, mock.Anything, mock.Anything).Return([]config.PurchaseExecution{}, nil)

		actionableHistoryRequest(t, store, []string{"*"}, map[string]string{})

		store.AssertNumberOfCalls(t, "GetExecutionsByStatuses", 1)
	})
}

// actionablePlanUUID is the plan the plan-history view filters on.
const actionablePlanUUID = "33333333-3333-4333-8333-333333333333"

func isPlanQuery(planID string) func(config.ExecutionsByStatusesQuery) bool {
	return func(q config.ExecutionsByStatusesQuery) bool { return isActionableQuery(q) && q.PlanID == planID }
}

// Issues #632 and #1404: the plan-history view's plan_id is part of the
// actionable SQL read, so MaxListLimit newer pending rows of other plans do
// not hide the plan's own pending row. The plan-less read returns the other
// plans' rows; the read carrying the plan returns the plan's row.
func TestHandleRequest_History_PlanPendingNotCrowdedOutByOtherPlans(t *testing.T) {
	now := time.Now()
	others := make([]config.PurchaseExecution, config.MaxListLimit)
	for i := range others {
		others[i] = pendingForAccount("other", actionableDevUUID, now)
		others[i].PlanID = "44444444-4444-4444-8444-444444444444"
	}
	mine := pendingForAccount("mine", actionableDevUUID, now.Add(-time.Hour))
	mine.PlanID = actionablePlanUUID

	store := new(MockConfigStore)
	store.On("GetExecutionsByStatusesFiltered", mock.Anything, mock.MatchedBy(isPlanQuery(""))).Return(others, nil).Maybe()
	store.On("GetExecutionsByStatusesFiltered", mock.Anything, mock.MatchedBy(isPlanQuery(actionablePlanUUID))).
		Return([]config.PurchaseExecution{mine}, nil)
	store.On("GetExecutionsByStatusesFiltered", mock.Anything, mock.MatchedBy(isStaleSweepQuery)).Return([]config.PurchaseExecution{}, nil).Maybe()

	body := actionableHistoryRequest(t, store, []string{"*"}, map[string]string{"plan_id": actionablePlanUUID})

	ids := make([]string, 0, len(body.Purchases))
	for _, p := range body.Purchases {
		ids = append(ids, p.PurchaseID)
	}
	assert.Equal(t, []string{"mine"}, ids)
	assert.False(t, body.Truncated, "the plan's class is not capped")
}

// A plan view narrows the actionable read, so it must still issue the
// separate unfiltered stale sweep: other plans' stale approvals are expired by
// any History load, plan view or not.
func TestHandleRequest_History_PlanViewStillSweepsAllStaleApprovals(t *testing.T) {
	var sweep config.ExecutionsByStatusesQuery
	store := new(MockConfigStore)
	store.On("GetExecutionsByStatusesFiltered", mock.Anything, mock.MatchedBy(isActionableQuery)).Return([]config.PurchaseExecution{}, nil)
	store.On("GetExecutionsByStatusesFiltered", mock.Anything, mock.MatchedBy(isStaleSweepQuery)).
		Run(func(args mock.Arguments) { sweep = args.Get(1).(config.ExecutionsByStatusesQuery) }).
		Return([]config.PurchaseExecution{}, nil).Once()

	actionableHistoryRequest(t, store, []string{"*"}, map[string]string{"plan_id": actionablePlanUUID})

	store.AssertExpectations(t)
	assert.Empty(t, sweep.PlanID, "the sweep is not limited to the viewed plan")
	assert.Nil(t, sweep.Scope)
	assert.Empty(t, sweep.Provider)
	assert.Empty(t, sweep.AccountIDs)
}

func TestIsNarrowed_PlanID(t *testing.T) {
	assert.False(t, isNarrowed(&config.ExecutionsByStatusesQuery{Statuses: actionableExecutionStatuses}))
	assert.True(t, isNarrowed(&config.ExecutionsByStatusesQuery{Statuses: actionableExecutionStatuses, PlanID: actionablePlanUUID}))
}

func TestActionableExecutionsQuery_AccountValues(t *testing.T) {
	f := historyFilters{
		AccountIDs:            []string{actionableDevUUID},
		ExternalIDsByProvider: map[string][]string{"aws": {"111111111111"}, "": {"333333333333"}},
	}
	q := actionableExecutionsQuery(&f, &historyScope{allowed: auth.UnrestrictedScope()})
	assert.ElementsMatch(t, []string{actionableDevUUID, "111111111111", "333333333333"}, q.AccountIDs)
	assert.Nil(t, q.Start)
	assert.Nil(t, q.End)
}

func TestHistoryScope_ExecutionScope(t *testing.T) {
	tests := []struct {
		name  string
		scope auth.AccountScope
		want  *config.ExecutionScope
	}{
		{"unrestricted", auth.UnrestrictedScope(), nil},
		{"empty denies attributed rows", auth.ScopeForAccounts(nil), &config.ExecutionScope{UserID: "u-1"}},
		{"by id", auth.ScopeForAccounts([]string{actionableDevUUID}),
			&config.ExecutionScope{UserID: "u-1", AccountIDs: []string{actionableDevUUID, "111111111111"}}},
		{"by unique name", auth.ScopeForAccounts([]string{"dev"}).WithRegisteredNames([]string{"dev", "other"}),
			&config.ExecutionScope{UserID: "u-1", AccountIDs: []string{"dev", actionableDevUUID, "111111111111"}}},
		{"literal external number", auth.ScopeForAccounts([]string{"222222222222"}),
			&config.ExecutionScope{UserID: "u-1", AccountIDs: []string{"222222222222"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := historyScope{allowed: tt.scope, accounts: actionableTestAccounts, userID: "u-1"}
			assert.Equal(t, tt.want, s.executionScope())
		})
	}
}
