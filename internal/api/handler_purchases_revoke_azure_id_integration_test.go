//go:build integration

package api

import (
	"context"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// TestRevokePurchase_AzureARMPurchaseIDOnRealPostgres is the regression test
// for issue #1102 item 5. revokePurchase first asks GetExecutionByID whether
// the ID is a scheduled execution. An Azure purchase ID is an ARM path, which
// Postgres refused to cast to the uuid execution_id column (SQLSTATE 22P02), so
// every Azure revoke failed with a 500 before reaching purchase_history. The
// unit tests mocked that lookup as ErrNotFound and never saw it.
//
// The store and the purchase_history row are real; only the two Azure SDK
// clients are stubs, so no cloud call is made.
func TestRevokePurchase_AzureARMPurchaseIDOnRealPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	f := newCreateConcurrencyFixture(ctx, t)

	var acctID string
	require.NoError(t, f.pool.QueryRow(ctx,
		"SELECT id::text FROM cloud_accounts WHERE external_id = 'fixture-subscription-a'").Scan(&acctID))

	const purchaseID = "/subscriptions/00000000-0000-0000-0000-00000000000a/providers/Microsoft.Capacity/reservationOrders/order-1102/reservations/res-1102"
	purchasedAt := time.Now().UTC().Add(-24 * time.Hour)
	require.NoError(t, f.store.SavePurchaseHistory(ctx, &config.PurchaseHistoryRecord{
		AccountID: "fixture-subscription-a", CloudAccountID: &acctID, PurchaseID: purchaseID,
		Timestamp: purchasedAt, Provider: "azure", Service: "compute", Region: "eastus",
		ResourceType: "Standard_D2s_v3", Count: 1, Term: 1, Payment: "monthly",
		RevocationWindowClosesAt: config.RevocationWindowClosesAtFor("azure", purchasedAt),
	}))

	returnClient := &stubReturnClient{}
	f.handler.azureRevokeFactory = &azureRevokeClientFactory{
		newCredential: func() (azcore.TokenCredential, error) { return nil, nil },
		newCalculateRefundClient: func(azcore.TokenCredential) (azureCalculateRefundClient, error) {
			return &stubCalcRefundClientWithAmount{amount: 42.5, currency: "USD", sessID: "sess-1102"}, nil
		},
		newReturnClient: func(azcore.TokenCredential) (azureReturnClient, error) { return returnClient, nil },
	}
	req := func(body string) *events.LambdaFunctionURLRequest {
		return &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer admin-token"}, Body: body}
	}

	t.Run("unknown ARM id is a 404, not a 500", func(t *testing.T) {
		_, err := f.handler.revokePurchase(ctx, req(""), "/providers/Microsoft.Capacity/reservationOrders/no-such-order/reservations/none")
		require.Error(t, err)
		ce, ok := IsClientError(err)
		require.True(t, ok, "want a client error, got %v", err)
		assert.Equal(t, 404, ce.code)
		assert.Zero(t, returnClient.calls)
	})

	t.Run("Azure reservation is returned and recorded", func(t *testing.T) {
		result, err := f.handler.revokePurchase(ctx,
			req(`{"expected_refund_amount":42.5,"expected_refund_currency":"USD"}`), purchaseID)
		require.NoError(t, err)
		res, ok := result.(*revokePurchaseResult)
		require.True(t, ok, "got %T", result)
		assert.Equal(t, "revoked", res.Status)
		assert.Equal(t, 1, returnClient.calls)

		var revokedAt *time.Time
		require.NoError(t, f.pool.QueryRow(ctx,
			"SELECT revoked_at FROM purchase_history WHERE purchase_id = $1", purchaseID).Scan(&revokedAt))
		assert.NotNil(t, revokedAt, "the revoke must be persisted on the purchase_history row")
	})
}
