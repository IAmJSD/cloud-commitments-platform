package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestGetHistory_SummaryComesFromScopedAggregate checks that the summary is
// built from the store aggregate (issue #675), with the caller's account scope
// applied to every group: tenant B's groups and another user's ambient
// execution must not reach a user scoped to tenant A.
func TestGetHistory_SummaryComesFromScopedAggregate(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockStore.On("ListPurchaseHistoryPage", ctx, mock.Anything, mock.Anything).Return([]config.PurchaseHistoryRecord{}, nil)
	mockStore.On("GetExecutionsByStatuses", ctx, mock.Anything, mock.Anything).Return([]config.PurchaseExecution{}, nil)
	mockStore.ListCloudAccountsFn = func(context.Context, config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return permsAccountList(), nil
	}
	accA, accB, other := permsAccA, permsAccB, "dddddddd-dddd-4ddd-dddd-dddddddddddd"
	own := permsScopedUserID
	upA, upB := 120.0, 9000.0
	recsA := []config.RecommendationRecord{{Provider: "aws", CloudAccountID: &accA}}
	recsB := []config.RecommendationRecord{{Provider: "aws", CloudAccountID: &accB}}
	groups := &config.HistorySummaryGroups{
		Purchases: []config.PurchaseHistorySummaryGroup{
			{AccountID: accA, Provider: "aws", Count: 150, UpfrontCost: &upA, EstimatedSavings: 30},
			{AccountID: accA, Provider: "aws", Revoked: true, Count: 4, UpfrontCost: &upB, EstimatedSavings: 900},
			{AccountID: accB, Provider: "aws", Count: 400, UpfrontCost: &upB, EstimatedSavings: 900},
		},
		Executions: []config.ExecutionSummaryGroup{
			{Status: "pending", CloudAccountID: &accA, Recommendations: recsA, Count: 7},
			{Status: "running", Recommendations: recsA, Count: 2}, // web bulk: account on the rec only
			{Status: "completed", CloudAccountID: &accA, Recommendations: recsA, Count: 3},
			{Status: config.StatusCanceled, CloudAccountID: &accA, Recommendations: recsA, Count: 110},
			{Status: "failed", CloudAccountID: &accB, Recommendations: recsB, Count: 50},
			{Status: "expired", CreatedByUserID: &own, Count: 1},
			{Status: "expired", CreatedByUserID: &other, Count: 6},
		},
	}
	var gotFilter config.HistorySummaryFilter
	mockStore.On("HistorySummaryAggregate", ctx, mock.Anything).
		Run(func(args mock.Arguments) { gotFilter = args.Get(1).(config.HistorySummaryFilter) }).
		Return(groups, nil).Once()

	handler := &Handler{auth: scopedAuthMock(t, ctx), config: mockStore}
	result, err := handler.getHistory(ctx, scopedReq(), map[string]string{})
	require.NoError(t, err)
	resp := result.(HistoryResponse)

	assert.True(t, resp.SummaryComplete)
	assert.Equal(t, 150+4+7+2+3+110+1, resp.Summary.TotalPurchases)
	assert.Equal(t, 150+3, resp.Summary.TotalCompleted, "purchases plus audit-gap executions")
	assert.Equal(t, 4, resp.Summary.TotalRevoked)
	assert.Equal(t, 7, resp.Summary.TotalPending)
	assert.Equal(t, 2, resp.Summary.TotalInProgress)
	assert.Equal(t, 0, resp.Summary.TotalFailed, "tenant B's failures must not leak")
	assert.Equal(t, 1, resp.Summary.TotalExpired, "only the caller's own ambient execution")
	require.NotNil(t, resp.Summary.TotalUpfront)
	assert.InDelta(t, upA, *resp.Summary.TotalUpfront, 1e-9)
	assert.InDelta(t, 30.0, resp.Summary.TotalMonthlySavings, 1e-9)
	assert.InDelta(t, 360.0, resp.Summary.TotalAnnualSavings, 1e-9)
	assert.ElementsMatch(t, historyExecutionStatuses(), gotFilter.ExecutionStatuses)
}

// TestGetHistory_SummaryFilterMatchesRowFetchFilter fails if the handler ever
// hands the aggregate a different purchase_history filter than the row fetch.
func TestGetHistory_SummaryFilterMatchesRowFetchFilter(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	var rowFilter config.PurchaseHistoryFilter
	mockStore.On("ListPurchaseHistoryPage", ctx, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { rowFilter = args.Get(1).(config.PurchaseHistoryFilter) }).
		Return([]config.PurchaseHistoryRecord{}, nil)
	mockStore.On("GetExecutionsByStatuses", ctx, mock.Anything, mock.Anything).Return([]config.PurchaseExecution{}, nil)
	var aggFilter config.HistorySummaryFilter
	mockStore.On("HistorySummaryAggregate", ctx, mock.Anything).
		Run(func(args mock.Arguments) { aggFilter = args.Get(1).(config.HistorySummaryFilter) }).
		Return(&config.HistorySummaryGroups{}, nil)

	mockAuth, req := adminHistoryReq(t, ctx)
	handler := &Handler{auth: mockAuth, config: mockStore}
	_, err := handler.getHistory(ctx, req, map[string]string{
		"provider": "aws", "start": "2026-01-01", "end": "2026-01-31", "limit": "7",
	})
	require.NoError(t, err)

	want := rowFilter
	want.Limit = aggFilter.History.Limit
	assert.Equal(t, want, aggFilter.History, "row fetch and aggregate must use one purchase_history filter")
	require.NotNil(t, aggFilter.ExecutionStart)
	assert.Equal(t, *rowFilter.Start, *aggFilter.ExecutionStart)
	assert.Equal(t, *rowFilter.End, *aggFilter.ExecutionEnd)
}

// TestGetHistory_SummaryFallsBackToPageWhenAggregateFails keeps History
// usable when the aggregate errors: the summary covers the listed rows and
// SummaryComplete says so.
func TestGetHistory_SummaryFallsBackToPageWhenAggregateFails(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	up := 40.0
	mockStore.On("ListPurchaseHistoryPage", ctx, mock.Anything, mock.Anything).Return([]config.PurchaseHistoryRecord{
		{PurchaseID: "p1", AccountID: "acct", UpfrontCost: &up, EstimatedSavings: 4},
	}, nil)
	mockStore.On("GetExecutionsByStatuses", ctx, mock.Anything, mock.Anything).Return([]config.PurchaseExecution{}, nil)
	mockStore.On("HistorySummaryAggregate", ctx, mock.Anything).Return(nil, errors.New("db down"))

	mockAuth, req := adminHistoryReq(t, ctx)
	handler := &Handler{auth: mockAuth, config: mockStore}
	result, err := handler.getHistory(ctx, req, map[string]string{})
	require.NoError(t, err)
	resp := result.(HistoryResponse)
	assert.False(t, resp.SummaryComplete)
	assert.Equal(t, 1, resp.Summary.TotalPurchases)
	require.NotNil(t, resp.Summary.TotalUpfront)
	assert.InDelta(t, up, *resp.Summary.TotalUpfront, 1e-9)
}

// TestGetHistory_LambdaSweepRunsAfterSummaryAggregate pins the ordering: on
// Lambda the stale-approval sweep runs synchronously, and if it ran before the
// aggregate the cards would count as expired a row the list shows as pending.
func TestGetHistory_LambdaSweepRunsAfterSummaryAggregate(t *testing.T) {
	ctx := context.Background()
	const staleID = "stale-exec"
	stale := config.PurchaseExecution{
		ExecutionID: staleID, Status: "pending", ScheduledDate: time.Now().Add(-8 * 24 * time.Hour),
		Recommendations: []config.RecommendationRecord{{Provider: "aws", Service: "rds"}},
	}
	var mu sync.Mutex
	var order []string
	note := func(s string) func(mock.Arguments) {
		return func(mock.Arguments) { mu.Lock(); order = append(order, s); mu.Unlock() }
	}
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockStore.On("ListPurchaseHistoryPage", ctx, mock.Anything, mock.Anything).Return([]config.PurchaseHistoryRecord{}, nil)
	mockStore.On("GetExecutionsByStatuses", ctx, mock.Anything, mock.Anything).Return([]config.PurchaseExecution{stale}, nil)
	mockStore.On("HistorySummaryAggregate", ctx, mock.Anything).Run(note("aggregate")).
		Return(&config.HistorySummaryGroups{Executions: []config.ExecutionSummaryGroup{{Status: "pending", Count: 1}}}, nil)
	mockStore.On("ExpireExecutionAtomic", mock.Anything, mock.Anything, staleID).Run(note("expire")).Return(true, nil).Once()

	mockAuth, req := adminHistoryReq(t, ctx)
	handler := &Handler{auth: mockAuth, config: mockStore, isLambda: true}
	result, err := handler.getHistory(ctx, req, map[string]string{})
	require.NoError(t, err)
	assert.Equal(t, []string{"aggregate", "expire"}, order)
	assert.Equal(t, 1, result.(HistoryResponse).Summary.TotalPending)
}

// TestAddHistoryRowToSummary_ExecutionRowsCarryNoDollars backs the aggregate's
// shortcut of rebuilding execution rows without their costs: no execution
// status the History view lists may add dollars to the totals. If one ever
// does, the aggregate must start summing execution dollars too.
func TestAddHistoryRowToSummary_ExecutionRowsCarryNoDollars(t *testing.T) {
	for _, status := range historyExecutionStatuses() {
		exec := config.PurchaseExecution{
			Status: status, Error: "x", TotalUpfrontCost: 100, EstimatedSavings: 10,
			Recommendations: []config.RecommendationRecord{{Provider: "aws", UpfrontCost: 100, Savings: 10}},
		}
		row := executionToHistoryRow(exec, "", "")
		summary := HistorySummary{TotalUpfront: new(float64)}
		addHistoryRowToSummary(&summary, &row, 3)
		assert.Equal(t, 3, summary.TotalPurchases, status)
		require.NotNil(t, summary.TotalUpfront, status)
		assert.Zero(t, *summary.TotalUpfront, status)
		assert.Zero(t, summary.TotalMonthlySavings, status)
	}
}

// TestAddHistoryRowToSummary_GroupMatchesRows checks that adding a group of n
// rows equals adding the n rows one at a time, including an unknown upfront
// cost making the total unknown.
func TestAddHistoryRowToSummary_GroupMatchesRows(t *testing.T) {
	a, b := 10.5, 4.25
	rows := []config.PurchaseHistoryRecord{
		{Status: "completed", UpfrontCost: &a, EstimatedSavings: 1},
		{Status: "completed", UpfrontCost: &b, EstimatedSavings: 2},
	}
	sum := a + b
	grouped := HistorySummary{TotalUpfront: new(float64)}
	addHistoryRowToSummary(&grouped, &config.PurchaseHistoryRecord{Status: "completed", UpfrontCost: &sum, EstimatedSavings: 3}, 2)
	grouped.TotalAnnualSavings = grouped.TotalMonthlySavings * 12
	assert.Equal(t, summarizePurchaseHistory(rows), grouped)

	unknown := HistorySummary{TotalUpfront: new(float64)}
	addHistoryRowToSummary(&unknown, &config.PurchaseHistoryRecord{Status: "completed", EstimatedSavings: 3}, 2)
	assert.Nil(t, unknown.TotalUpfront)
}
