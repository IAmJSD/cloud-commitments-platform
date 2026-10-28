//go:build integration

package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRevokeAzure_UncertainReturn_FlagKeptUntilSweepReleases covers issue
// #1529 on real Postgres, from the revoke request to the sweep. Azure answers
// the Return with a 503, which does not say whether it took the Return:
//
//   - the row stays flagged in flight with no recorded Return and not revoked,
//     and the user is told the outcome is uncertain (before the fix the flag
//     was cleared and the answer was a flat failure);
//   - a retry is answered RECONCILE_PENDING and posts no second Return;
//   - the finalize_revocations sweep leaves the fresh flag alone, and once it
//     is older than the 2h stale window releases it without marking the
//     purchase revoked.
//
// Stub Azure clients; nothing calls Azure.
func TestRevokeAzure_UncertainReturn_FlagKeptUntilSweepReleases(t *testing.T) {
	pg, store := setupSubmitDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	record := withinWindowRecord("/providers/Microsoft.Capacity/reservationOrders/order-1529/reservations/res-1529")
	record.AccountID = "acct-1529"
	record.Region = "eastus"
	record.ResourceType = "Standard_D2s_v3"
	require.NoError(t, store.SavePurchaseHistory(ctx, record))

	calc := &stubCalcRefundClientWithAmount{amount: 10.0, currency: "USD", sessID: "s-1529"}
	ret := &stubReturnClient{err: &azcore.ResponseError{StatusCode: 503, ErrorCode: "ServiceUnavailable"}}
	h := &Handler{config: store, azureRevokeFactory: &azureRevokeClientFactory{
		newCredential:            func() (azcore.TokenCredential, error) { return nil, nil },
		newCalculateRefundClient: func(azcore.TokenCredential) (azureCalculateRefundClient, error) { return calc, nil },
		newReturnClient:          func(azcore.TokenCredential) (azureReturnClient, error) { return ret, nil },
	}}
	body, err := json.Marshal(confirmedQuote(10, "USD"))
	require.NoError(t, err)
	req := &events.LambdaFunctionURLRequest{Body: string(body)}

	type rowState struct {
		returnPostedAt, revokedAt *time.Time
		inFlight                  bool
	}
	load := func() rowState {
		t.Helper()
		var s rowState
		require.NoError(t, pg.DB.Pool().QueryRow(ctx, `
			SELECT return_posted_at, revoked_at, revocation_in_flight
			  FROM purchase_history WHERE purchase_id = $1`, record.PurchaseID).
			Scan(&s.returnPostedAt, &s.revokedAt, &s.inFlight))
		return s
	}

	_, err = h.loadAndRevokePurchaseHistory(ctx, req, revokeAdminSession(), record.PurchaseID)
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "want the uncertain-outcome ClientError, got %v", err)
	assert.Equal(t, 500, ce.code)
	assert.Equal(t, errRevokeReturnUncertain, ce.message)
	got := load()
	assert.True(t, got.inFlight, "an uncertain Return must keep the in-flight flag")
	assert.Nil(t, got.returnPostedAt, "an uncertain Return is not recorded as accepted")
	assert.Nil(t, got.revokedAt, "an uncertain Return does not revoke the purchase")

	result, err := h.loadAndRevokePurchaseHistory(ctx, req, revokeAdminSession(), record.PurchaseID)
	require.NoError(t, err)
	pending, ok := result.(*revokeReconcilePendingResult)
	require.True(t, ok, "want RECONCILE_PENDING on retry, got %T %+v", result, result)
	assert.Equal(t, "RECONCILE_PENDING", pending.Code)
	assert.Equal(t, 1, ret.calls, "the retry must not post a second Return")

	sweeper := purchase.NewManager(purchase.ManagerConfig{ConfigStore: store})
	_, err = sweeper.FinalizeInFlightRevocations(ctx)
	require.NoError(t, err)
	got = load()
	assert.True(t, got.inFlight, "the sweep leaves a fresh uncertain flag alone")
	assert.Nil(t, got.revokedAt, "the sweep must not revoke a purchase with no recorded Return")

	_, err = pg.DB.Pool().Exec(ctx, `
		UPDATE purchase_history
		   SET revocation_in_flight_at = now() - interval '2 hours 1 minute'
		 WHERE purchase_id = $1`, record.PurchaseID)
	require.NoError(t, err)
	res, err := sweeper.FinalizeInFlightRevocations(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, res.Released, 1)
	got = load()
	assert.False(t, got.inFlight, "past the stale window the sweep releases the flag")
	assert.Nil(t, got.revokedAt, "a released row is not revoked")
	assert.Nil(t, got.returnPostedAt)
}
