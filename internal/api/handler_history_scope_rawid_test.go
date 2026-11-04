package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// historyScopeResponse runs GET /api/history as a user whose allow-list is
// `scope`, with rows for the list and groups for the SQL summary totals.
func historyScopeResponse(t *testing.T, scope []string, accounts []config.CloudAccount, rows []config.PurchaseHistoryRecord, groups []config.PurchaseHistorySummaryGroup) HistoryResponse {
	t.Helper()
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, "tok").Return(&Session{UserID: "u-1", Email: "u@example.com"}, nil)
	mockAuth.On("HasPermissionAPI", mock.Anything, "u-1", mock.Anything, mock.Anything).Return(true, nil)
	mockAuth.On("GetAllowedAccountsAPI", mock.Anything, "u-1").Return(scope, nil)

	store := new(MockConfigStore)
	pageRows := make([]config.PurchaseHistoryPageRow, len(rows))
	for i := range rows {
		pageRows[i] = config.PurchaseHistoryPageRow{PurchaseHistoryRecord: rows[i], RowID: fmt.Sprintf("row-%d", i)}
	}
	store.On("ListPurchaseHistoryPage", mock.Anything, mock.Anything, mock.Anything).Return(pageRows, nil)
	store.On("ListHistoryExecutionsPage", mock.Anything, mock.Anything).Return([]config.PurchaseExecution{}, nil)
	store.On("GetExecutionsByStatuses", mock.Anything, mock.Anything, mock.Anything).Return([]config.PurchaseExecution{}, nil)
	store.On("HistorySummaryAggregate", mock.Anything, mock.Anything).Return(&config.HistorySummaryGroups{Purchases: groups}, nil)
	store.ListCloudAccountsFn = func(_ context.Context, _ config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return accounts, nil
	}
	h := &Handler{auth: mockAuth, config: store}
	resp, err := h.HandleRequest(context.Background(), &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"Authorization": "Bearer tok"},
		QueryStringParameters: map[string]string{},
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

// A raw external-id grant names one provider's account. Another provider's
// rows with the same number, registered or not, are outside it, in the list
// and in the summary totals.
func TestHandleRequest_History_RawExternalIDGrantIsProviderScoped(t *testing.T) {
	t.Parallel()
	awsAcct := config.CloudAccount{ID: "u-aws", Name: "dev", Provider: "aws", ExternalID: "111"}
	up := 10.0
	rows := []config.PurchaseHistoryRecord{
		{PurchaseID: "aws-row", Provider: "aws", AccountID: "111", Status: "completed"},
		{PurchaseID: "gcp-row", Provider: "gcp", AccountID: "111", Status: "completed"},
	}
	groups := []config.PurchaseHistorySummaryGroup{
		{AccountID: "111", Provider: "aws", Count: 2, UpfrontCost: &up},
		{AccountID: "111", Provider: "gcp", Count: 5, UpfrontCost: &up},
	}

	t.Run("unregistered other-provider id", func(t *testing.T) {
		t.Parallel()
		got := historyScopeResponse(t, []string{"111"}, []config.CloudAccount{awsAcct}, rows, groups)
		require.Len(t, got.Purchases, 1)
		assert.Equal(t, "aws-row", got.Purchases[0].PurchaseID)
		assert.Equal(t, 2, got.Summary.TotalPurchases)
	})

	t.Run("registered on both providers", func(t *testing.T) {
		t.Parallel()
		gcpAcct := config.CloudAccount{ID: "u-gcp", Name: "other", Provider: "gcp", ExternalID: "111"}
		got := historyScopeResponse(t, []string{"u-aws"}, []config.CloudAccount{awsAcct, gcpAcct}, rows, groups)
		require.Len(t, got.Purchases, 1, "a UUID grant covers the owning provider's rows by external id, not the other provider's")
		assert.Equal(t, "aws-row", got.Purchases[0].PurchaseID)
		assert.Equal(t, 2, got.Summary.TotalPurchases)
	})

	t.Run("legacy row without provider on a registered id still matches", func(t *testing.T) {
		t.Parallel()
		legacy := []config.PurchaseHistoryRecord{{PurchaseID: "legacy", AccountID: "111", Status: "completed"}}
		got := historyScopeResponse(t, []string{"111"}, []config.CloudAccount{awsAcct}, legacy, nil)
		require.Len(t, got.Purchases, 1)
	})

	t.Run("id no registered account has stays a literal grant", func(t *testing.T) {
		t.Parallel()
		got := historyScopeResponse(t, []string{"111"}, nil, rows, groups)
		assert.Len(t, got.Purchases, 2)
		assert.Equal(t, 7, got.Summary.TotalPurchases)
	})
}
