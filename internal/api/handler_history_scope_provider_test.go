package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// historyScopeIDs drives GET /api/history through HandleRequest as a session
// user whose allow-list is `scope`, with the given accounts registered, and
// returns the purchase ids that come back.
func historyScopeIDs(t *testing.T, scope []string, accounts []config.CloudAccount, rows []config.PurchaseHistoryRecord) []string {
	t.Helper()
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, "tok").Return(&Session{UserID: "u-1", Email: "u@example.com"}, nil)
	mockAuth.On("HasPermissionAPI", mock.Anything, "u-1", mock.Anything, mock.Anything).Return(true, nil)
	mockAuth.On("GetAllowedAccountsAPI", mock.Anything, "u-1").Return(scope, nil)

	store := new(MockConfigStore)
	store.On("GetAllPurchaseHistory", mock.Anything, mock.Anything).Return(append([]config.PurchaseHistoryRecord(nil), rows...), nil)
	store.On("GetExecutionsByStatuses", mock.Anything, mock.Anything, mock.Anything).Return([]config.PurchaseExecution{}, nil)
	store.ListCloudAccountsFn = func(_ context.Context, _ config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return accounts, nil
	}

	h := &Handler{auth: mockAuth, config: store}
	resp, err := h.HandleRequest(ctx, &events.LambdaFunctionURLRequest{
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
	ids := make([]string, 0, len(body.Purchases))
	for _, p := range body.Purchases {
		ids = append(ids, p.PurchaseID)
	}
	return ids
}

// Issue #550: external ids are unique per (provider, external_id) only. A
// History user scoped to an account must see rows of the account in their scope and
// none of the same-external-id account under another provider, whatever the
// order the accounts are listed in.
func TestHandleRequest_History_SameExternalIDAcrossProviders(t *testing.T) {
	t.Parallel()
	awsDev := config.CloudAccount{ID: "u-aws", Name: "dev", Provider: "aws", ExternalID: "123"}
	azProd := config.CloudAccount{ID: "u-az", Name: "prod", Provider: "azure", ExternalID: "123"}
	awsRow := config.PurchaseHistoryRecord{PurchaseID: "aws-row", Provider: "aws", AccountID: "123", Status: "completed"}
	azRow := config.PurchaseHistoryRecord{PurchaseID: "az-row", Provider: "azure", AccountID: "123", Status: "completed"}
	rows := []config.PurchaseHistoryRecord{awsRow, azRow}
	bothOrders := map[string][]config.CloudAccount{
		"aws listed first":   {awsDev, azProd},
		"azure listed first": {azProd, awsDev},
	}

	for order, accounts := range bothOrders {
		t.Run("scope names azure account, "+order, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, []string{"az-row"}, historyScopeIDs(t, []string{"u-az"}, accounts, rows))
		})
		t.Run("scope names aws account, "+order, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, []string{"aws-row"}, historyScopeIDs(t, []string{"u-aws"}, accounts, rows))
		})
	}

	t.Run("legacy row without provider on an ambiguous external id is not name-matched", func(t *testing.T) {
		t.Parallel()
		legacy := []config.PurchaseHistoryRecord{{PurchaseID: "legacy", AccountID: "123", Status: "completed"}}
		assert.Empty(t, historyScopeIDs(t, []string{"u-az"}, []config.CloudAccount{awsDev, azProd}, legacy))
	})

	t.Run("a set cloud account id stays authoritative", func(t *testing.T) {
		t.Parallel()
		uuid := "u-az"
		row := []config.PurchaseHistoryRecord{{PurchaseID: "by-uuid", Provider: "aws", AccountID: "123", CloudAccountID: &uuid, Status: "completed"}}
		assert.Equal(t, []string{"by-uuid"}, historyScopeIDs(t, []string{"u-az"}, []config.CloudAccount{awsDev, azProd}, row))
		assert.Empty(t, historyScopeIDs(t, []string{"u-aws"}, []config.CloudAccount{awsDev, azProd}, row))
	})
}

// Unambiguous accounts keep matching by account ID or by a raw external-id
// entry, with and without a provider on the row.
func TestHandleRequest_History_UniqueExternalIDStillMatches(t *testing.T) {
	t.Parallel()
	in := config.CloudAccount{ID: "u-in", Name: "in", Provider: "aws", ExternalID: "111"}
	out := config.CloudAccount{ID: "u-out", Name: "out", Provider: "aws", ExternalID: "222"}
	rows := []config.PurchaseHistoryRecord{
		{PurchaseID: "in-prov", Provider: "aws", AccountID: "111", Status: "completed"},
		{PurchaseID: "in-legacy", AccountID: "111", Status: "completed"},
		{PurchaseID: "out", Provider: "aws", AccountID: "222", Status: "completed"},
	}
	got := historyScopeIDs(t, []string{"u-in"}, []config.CloudAccount{in, out}, rows)
	assert.ElementsMatch(t, []string{"in-prov", "in-legacy"}, got)
	got = historyScopeIDs(t, []string{"111"}, []config.CloudAccount{in, out}, rows)
	assert.ElementsMatch(t, []string{"in-prov", "in-legacy"}, got)
}

// In-flight history rows carry the account UUID in AccountID with no
// CloudAccountID, so a scoped user must still see their own rows.
func TestHandleRequest_History_UUIDInAccountID(t *testing.T) {
	t.Parallel()
	dev := config.CloudAccount{ID: "u-aws", Name: "dev", Provider: "aws", ExternalID: "123"}
	prod := config.CloudAccount{ID: "u-az", Name: "prod", Provider: "azure", ExternalID: "123"}
	accounts := []config.CloudAccount{dev, prod}

	t.Run("pending row with provider", func(t *testing.T) {
		t.Parallel()
		rows := []config.PurchaseHistoryRecord{{PurchaseID: "p1", Provider: "aws", AccountID: "u-aws", Status: "pending"}}
		assert.Equal(t, []string{"p1"}, historyScopeIDs(t, []string{"u-aws"}, accounts, rows))
		assert.Empty(t, historyScopeIDs(t, []string{"u-az"}, accounts, rows))
	})
	t.Run("pending row without provider", func(t *testing.T) {
		t.Parallel()
		rows := []config.PurchaseHistoryRecord{{PurchaseID: "p2", AccountID: "u-aws", Status: "pending"}}
		assert.Equal(t, []string{"p2"}, historyScopeIDs(t, []string{"u-aws"}, accounts, rows))
		assert.Empty(t, historyScopeIDs(t, []string{"u-az"}, accounts, rows))
	})
}

// Two accounts of one provider sharing an external id are ambiguous: the row
// is hidden from both account scopes.
func TestHandleRequest_History_SameProviderAmbiguousExternalID(t *testing.T) {
	t.Parallel()
	a := config.CloudAccount{ID: "u-a", Name: "dev", Provider: "aws", ExternalID: "123"}
	b := config.CloudAccount{ID: "u-b", Name: "prod", Provider: "aws", ExternalID: "123"}
	rows := []config.PurchaseHistoryRecord{{PurchaseID: "amb", Provider: "aws", AccountID: "123", Status: "completed"}}
	for _, scope := range []string{"u-a", "u-b"} {
		assert.Empty(t, historyScopeIDs(t, []string{scope}, []config.CloudAccount{a, b}, rows), scope)
		assert.Empty(t, historyScopeIDs(t, []string{scope}, []config.CloudAccount{b, a}, rows), scope)
	}
}
