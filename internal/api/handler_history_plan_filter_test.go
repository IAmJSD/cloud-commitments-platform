package api

import (
	"context"
	"strings"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const planFilterUUID = "11111111-1111-4111-8111-111111111111"

func TestParseHistoryFilters_PlanID(t *testing.T) {
	f, err := parseHistoryFilters(map[string]string{"plan_id": planFilterUUID})
	require.NoError(t, err)
	assert.Equal(t, planFilterUUID, f.PlanID)

	f, err = parseHistoryFilters(map[string]string{})
	require.NoError(t, err)
	assert.Empty(t, f.PlanID, "absent plan_id means no plan filter")

	f, err = parseHistoryFilters(map[string]string{"plan_id": "  " + strings.ToUpper(planFilterUUID) + "\t"})
	require.NoError(t, err)
	assert.Equal(t, planFilterUUID, f.PlanID, "padded and uppercase plan_id is canonicalised")

	for _, bad := range []string{"not-a-uuid", "' OR 1=1 --", "11111111-1111-4111-8111-11111111111"} {
		_, err = parseHistoryFilters(map[string]string{"plan_id": bad})
		require.Error(t, err, bad)
		ce, ok := IsClientError(err)
		require.True(t, ok, "got %T: %v", err, err)
		assert.Equal(t, 400, ce.code)
		assert.Contains(t, err.Error(), "invalid plan_id")
	}
}

func TestHistoryFilters_PlanIDMatchesExecution(t *testing.T) {
	f := historyFilters{PlanID: planFilterUUID}
	assert.True(t, f.matchesExecution(config.PurchaseExecution{PlanID: planFilterUUID}))
	assert.False(t, f.matchesExecution(config.PurchaseExecution{PlanID: "22222222-2222-4222-8222-222222222222"}))
	assert.False(t, f.matchesExecution(config.PurchaseExecution{}), "a planless execution is not in any plan's history")
	assert.True(t, historyFilters{}.matchesExecution(config.PurchaseExecution{PlanID: planFilterUUID}), "no plan filter keeps a plan's execution")
}

// TestGetHistory_PlanIDReachesStoreFilters checks that plan_id reaches both the
// row fetch and the summary aggregate, and that an absent plan_id sets neither.
func TestGetHistory_PlanIDReachesStoreFilters(t *testing.T) {
	for _, tc := range []struct{ name, planID string }{{"with plan", planFilterUUID}, {"without plan", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			mockStore := new(MockConfigStore)
			var rowFilter config.PurchaseHistoryFilter
			mockStore.On("ListPurchaseHistoryPage", ctx, mock.Anything, mock.Anything).
				Run(func(args mock.Arguments) { rowFilter = args.Get(1).(config.PurchaseHistoryFilter) }).
				Return([]config.PurchaseHistoryRecord{}, nil)
			mockStore.On("GetExecutionsByStatuses", ctx, mock.Anything, mock.Anything).Return([]config.PurchaseExecution{}, nil)
			var aggFilter config.HistorySummaryFilter
			mockStore.On("HistorySummaryAggregate", ctx, mock.Anything).
				Run(func(args mock.Arguments) { aggFilter = args.Get(1).(config.HistorySummaryFilter) }).
				Return(&config.HistorySummaryGroups{}, nil)

			mockAuth, req := adminHistoryReq(ctx)
			handler := &Handler{auth: mockAuth, config: mockStore}
			params := map[string]string{}
			if tc.planID != "" {
				params["plan_id"] = tc.planID
			}
			_, err := handler.getHistory(ctx, req, params)
			require.NoError(t, err)
			assert.Equal(t, tc.planID, rowFilter.PlanID)
			assert.Equal(t, tc.planID, aggFilter.History.PlanID)
		})
	}
}

func TestGetHistory_BadPlanIDIs400(t *testing.T) {
	ctx := context.Background()
	mockAuth, req := adminHistoryReq(ctx)
	handler := &Handler{auth: mockAuth, config: new(MockConfigStore)}
	_, err := handler.getHistory(ctx, req, map[string]string{"plan_id": "garbage"})
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, 400, ce.code)
}
