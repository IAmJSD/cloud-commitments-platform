package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// inventoryNames drives GET /api/inventory/commitments through HandleRequest
// as a session user whose allow-list is `scope` (nil means unrestricted) and
// returns purchase id -> account name for the rows that come back.
func inventoryNames(t *testing.T, scope []string, accounts []config.CloudAccount, rows []config.PurchaseHistoryRecord) map[string]string {
	t.Helper()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("ValidateSession", mock.Anything, "tok").Return(&Session{UserID: "u-1", Email: "u@example.com"}, nil)
	mockAuth.On("HasPermissionAPI", mock.Anything, "u-1", mock.Anything, mock.Anything).Return(true, nil)
	mockAuth.On("GetAllowedAccountsAPI", mock.Anything, "u-1").Return(scope, nil)

	store := new(MockConfigStore)
	t.Cleanup(func() { store.AssertExpectations(t) })
	store.On("GetActivePurchaseHistory", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(append([]config.PurchaseHistoryRecord(nil), rows...), nil)
	store.ListCloudAccountsFn = func(_ context.Context, _ config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return accounts, nil
	}

	h := &Handler{auth: mockAuth, config: store}
	resp, err := h.HandleRequest(context.Background(), &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"Authorization": "Bearer tok"},
		QueryStringParameters: map[string]string{},
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "GET", Path: "/api/inventory/commitments"},
		},
	})
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode, "body: %s", resp.Body)

	var body InventoryCommitmentsResponse
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &body))
	names := make(map[string]string, len(body.Commitments))
	for _, c := range body.Commitments {
		names[c.ID] = c.AccountName
	}
	return names
}

func activeRow(purchaseID, provider, accountID string) config.PurchaseHistoryRecord {
	return config.PurchaseHistoryRecord{
		PurchaseID: purchaseID,
		Provider:   provider,
		AccountID:  accountID,
		Service:    "ec2",
		Count:      1,
		Term:       3,
		Timestamp:  time.Now().AddDate(0, -1, 0),
		Status:     "completed",
	}
}

// Issue #520: external ids are unique per (provider, external_id) only. The
// inventory account name was keyed by external id alone, so with two accounts
// sharing an id the name on a row (and the scope decision made with it)
// depended on the order the accounts were listed in.
func TestHandleRequest_Inventory_SameExternalIDAcrossProviders(t *testing.T) {
	t.Parallel()
	awsDev := config.CloudAccount{ID: "u-aws", Name: "dev", Provider: "aws", ExternalID: "123"}
	azProd := config.CloudAccount{ID: "u-az", Name: "prod", Provider: "azure", ExternalID: "123"}
	rows := []config.PurchaseHistoryRecord{activeRow("aws-row", "aws", "123"), activeRow("az-row", "azure", "123")}
	bothOrders := map[string][]config.CloudAccount{
		"aws listed first":   {awsDev, azProd},
		"azure listed first": {azProd, awsDev},
	}

	for order, accounts := range bothOrders {
		t.Run("scope names aws account, "+order, func(t *testing.T) {
			t.Parallel()
			got := inventoryNames(t, []string{"u-aws"}, accounts, rows)
			assert.Equal(t, map[string]string{"123:aws-row": "dev"}, got)
		})
		t.Run("scope names azure account, "+order, func(t *testing.T) {
			t.Parallel()
			got := inventoryNames(t, []string{"u-az"}, accounts, rows)
			assert.Equal(t, map[string]string{"123:az-row": "prod"}, got)
		})
		t.Run("unrestricted sees each row under its own account name, "+order, func(t *testing.T) {
			t.Parallel()
			got := inventoryNames(t, nil, accounts, rows)
			assert.Equal(t, map[string]string{"123:aws-row": "dev", "123:az-row": "prod"}, got)
		})
	}
}

// A set CloudAccountID and an account UUID in AccountID stay authoritative,
// and an external id that is unique still resolves.
func TestHandleRequest_Inventory_ResolutionOrder(t *testing.T) {
	t.Parallel()
	awsDev := config.CloudAccount{ID: "u-aws", Name: "dev", Provider: "aws", ExternalID: "123"}
	azProd := config.CloudAccount{ID: "u-az", Name: "prod", Provider: "azure", ExternalID: "123"}
	solo := config.CloudAccount{ID: "u-solo", Name: "solo", Provider: "gcp", ExternalID: "777"}
	accounts := []config.CloudAccount{awsDev, azProd, solo}

	uuid := "u-az"
	byCloudID := activeRow("by-cloud-id", "aws", "123")
	byCloudID.CloudAccountID = &uuid
	rows := []config.PurchaseHistoryRecord{
		byCloudID,
		activeRow("by-uuid", "aws", "u-aws"),
		activeRow("unique", "gcp", "777"),
	}
	got := inventoryNames(t, nil, accounts, rows)
	assert.Equal(t, map[string]string{
		"123:by-cloud-id": "prod",
		"u-aws:by-uuid":   "dev",
		"777:unique":      "solo",
	}, got)
}

// Two accounts of one provider sharing an external id are ambiguous: the row
// is attributed to neither, so a scoped user does not see it and an
// unrestricted user sees it without a name.
func TestHandleRequest_Inventory_SameProviderAmbiguousExternalID(t *testing.T) {
	t.Parallel()
	a := config.CloudAccount{ID: "u-a", Name: "dev", Provider: "aws", ExternalID: "123"}
	b := config.CloudAccount{ID: "u-b", Name: "prod", Provider: "aws", ExternalID: "123"}
	rows := []config.PurchaseHistoryRecord{activeRow("amb", "aws", "123")}
	for _, accounts := range [][]config.CloudAccount{{a, b}, {b, a}} {
		for _, scope := range []string{"u-a", "u-b"} {
			assert.Empty(t, inventoryNames(t, []string{scope}, accounts, rows), scope)
		}
		assert.Equal(t, map[string]string{"123:amb": ""}, inventoryNames(t, nil, accounts, rows))
	}
}

// A failed account load is an error, not a blank account name.
func TestHandleRequest_Inventory_AccountListFailureIsAnError(t *testing.T) {
	t.Parallel()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("ValidateSession", mock.Anything, "tok").Return(&Session{UserID: "u-1", Email: "u@example.com"}, nil)
	mockAuth.On("HasPermissionAPI", mock.Anything, "u-1", mock.Anything, mock.Anything).Return(true, nil)
	mockAuth.On("GetAllowedAccountsAPI", mock.Anything, "u-1").Return([]string(nil), nil)
	store := new(MockConfigStore)
	t.Cleanup(func() { store.AssertExpectations(t) })
	store.On("GetActivePurchaseHistory", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]config.PurchaseHistoryRecord{activeRow("r", "aws", "123")}, nil)
	store.ListCloudAccountsFn = func(_ context.Context, _ config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return nil, assert.AnError
	}

	h := &Handler{auth: mockAuth, config: store}
	resp, _ := h.HandleRequest(context.Background(), &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"Authorization": "Bearer tok"},
		QueryStringParameters: map[string]string{},
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "GET", Path: "/api/inventory/commitments"},
		},
	})
	assert.GreaterOrEqual(t, resp.StatusCode, 500)
}
