//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	armreservations "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/reservations/armreservations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// revokeRequestKey tags each concurrent revoke's context with its index, so
// the stub Return client knows which request is calling it.
type revokeRequestKey struct{}

// barrierCalcRefundClient answers CalculateRefund with a fixed quote, but
// only once every party has called it. Both revokes have loaded the row (not
// revoked, not in flight) by then, so they reach the in-flight flip together,
// as two first-time revokes would.
type barrierCalcRefundClient struct {
	price   *armreservations.Price
	parties int32
	arrived atomic.Int32
	ready   chan struct{}
}

func (c *barrierCalcRefundClient) Post(ctx context.Context, _ string, _ armreservations.CalculateRefundRequest, _ *armreservations.CalculateRefundClientPostOptions) (armreservations.CalculateRefundClientPostResponse, error) {
	if c.arrived.Add(1) == c.parties {
		close(c.ready)
	}
	select {
	case <-c.ready:
	case <-ctx.Done():
		return armreservations.CalculateRefundClientPostResponse{}, ctx.Err()
	}
	return armreservations.CalculateRefundClientPostResponse{
		CalculateRefundResponse: armreservations.CalculateRefundResponse{
			Properties: &armreservations.RefundResponseProperties{
				SessionID:           toPtr("s-claim-race"),
				BillingRefundAmount: c.price,
			},
		},
	}, nil
}

// raceReturnClient stands in for Azure's Return API. The first Return
// succeeds, but only after the other request has finished, so the other
// request runs its whole path while the first one's refund is in flight.
// Azure refuses a second Return of the same reservation, so any later call
// gets a 400. While the first Return is in flight, it records whether the row
// still carries the in-flight flag.
type raceReturnClient struct {
	store      *config.PostgresStore
	purchaseID string
	done       [2]chan struct{}
	calls      atomic.Int32
	// flagHeld is whether the row was still flagged in flight once the other
	// request had finished, while the first Return was still in flight.
	flagHeld atomic.Bool
}

func (c *raceReturnClient) Post(ctx context.Context, _ string, _ armreservations.RefundRequest, _ *armreservations.ReturnClientPostOptions) (armreservations.ReturnClientPostResponse, error) {
	if c.calls.Add(1) > 1 {
		return armreservations.ReturnClientPostResponse{}, &azcore.ResponseError{StatusCode: 400, ErrorCode: "ReservationAlreadyReturned"}
	}
	me, _ := ctx.Value(revokeRequestKey{}).(int)
	select {
	case <-c.done[1-me]:
	case <-time.After(10 * time.Second):
		// The other request never finished; the assertions report it.
	case <-ctx.Done():
		return armreservations.ReturnClientPostResponse{}, ctx.Err()
	}
	row, err := c.store.GetPurchaseHistoryByPurchaseID(context.Background(), c.purchaseID)
	if err == nil && row != nil {
		c.flagHeld.Store(row.RevocationInFlight)
	}
	return armreservations.ReturnClientPostResponse{}, nil
}

// TestRevokeAzure_ConcurrentFirstRevokes_OneReturn covers issue #1518 on real
// Postgres. Two first-time revokes of the same Azure purchase load the row
// (unrevoked, not in flight) and reach the in-flight flip together. Before
// the fix the flip let an already-flagged row be flipped again, so both
// posted a Return; Azure refused the second, and that request's error
// handling cleared the flag while the first request's refund was still in
// flight. Had the first request's MarkPurchaseRevoked then failed, the row
// would have been left neither revoked nor flagged, out of the
// finalize_revocations sweep's reach.
//
// Now exactly one request posts a Return and records the revocation, the
// other answers RECONCILE_PENDING without posting one, and the flag stays set
// until the holder's MarkPurchaseRevoked clears it.
func TestRevokeAzure_ConcurrentFirstRevokes_OneReturn(t *testing.T) {
	_, store := setupSubmitDB(t)
	const rounds = 5
	for round := range rounds {
		t.Run(fmt.Sprintf("round %d", round), func(t *testing.T) {
			runConcurrentFirstRevokes(t, store, round)
		})
	}
}

func runConcurrentFirstRevokes(t *testing.T, store *config.PostgresStore, round int) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	record := withinWindowRecord(fmt.Sprintf(
		"/providers/Microsoft.Capacity/reservationOrders/order-1518-%d/reservations/res-1518-%d", round, round))
	record.AccountID = fmt.Sprintf("acct-1518-%d", round)
	record.Region = "eastus"
	record.ResourceType = "Standard_D2s_v3"
	require.NoError(t, store.SavePurchaseHistory(ctx, record))

	calc := &barrierCalcRefundClient{
		price:   &armreservations.Price{Amount: toPtr(10.0), CurrencyCode: toPtr("USD")},
		parties: 2,
		ready:   make(chan struct{}),
	}
	ret := &raceReturnClient{store: store, purchaseID: record.PurchaseID, done: [2]chan struct{}{make(chan struct{}), make(chan struct{})}}
	h := &Handler{config: store, azureRevokeFactory: &azureRevokeClientFactory{
		newCredential:            func() (azcore.TokenCredential, error) { return nil, nil },
		newCalculateRefundClient: func(azcore.TokenCredential) (azureCalculateRefundClient, error) { return calc, nil },
		newReturnClient:          func(azcore.TokenCredential) (azureReturnClient, error) { return ret, nil },
	}}
	body, err := json.Marshal(confirmedQuote(10, "USD"))
	require.NoError(t, err)
	req := &events.LambdaFunctionURLRequest{Body: string(body)}

	results := make([]any, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(ret.done[i])
			reqCtx := context.WithValue(ctx, revokeRequestKey{}, i)
			results[i], errs[i] = h.loadAndRevokePurchaseHistory(reqCtx, req, revokeAdminSession(), record.PurchaseID)
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(1), ret.calls.Load(), "exactly one of two concurrent first revokes may post an Azure Return")
	assert.True(t, ret.flagHeld.Load(), "the other request must leave the in-flight flag set while the first one's Return is in flight")

	revoked, inFlight := 0, 0
	for i := range 2 {
		require.NoError(t, errs[i], "request %d", i)
		switch res := results[i].(type) {
		case *revokePurchaseResult:
			assert.Equal(t, "revoked", res.Status, "request %d", i)
			revoked++
		case *revokeReconcilePendingResult:
			assert.Equal(t, revocationInFlightResult(), res, "request %d", i)
			inFlight++
		default:
			t.Errorf("request %d: unexpected result %T %+v", i, results[i], results[i])
		}
	}
	assert.Equal(t, 1, revoked, "one request revokes")
	assert.Equal(t, 1, inFlight, "the other answers in flight (RECONCILE_PENDING)")

	got, err := store.GetPurchaseHistoryByPurchaseID(ctx, record.PurchaseID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.NotNil(t, got.RevokedAt, "the winner's MarkPurchaseRevoked must land")
	assert.False(t, got.RevocationInFlight, "MarkPurchaseRevoked clears the holder's flag")
}

// TestRevokeAzure_RequestCanceledAfterReturn_RecordsAndRevokes covers issue
// #1525 on real Postgres. The request is canceled right after Azure accepts
// the Return (a client disconnect). Before the fix every write that follows
// ran under the canceled request context, so pgx refused each attempt: the
// Return went unrecorded, the row was never revoked, and the
// finalize_revocations sweep would release it after 2h as if nothing had been
// refunded. Now those writes run detached, so the row ends up with the Return
// recorded and revoked.
//
// It also covers issue #1527: the stub cancels the request while the Return is
// in flight and, like the real SDK call, reports a canceled context as an
// error even though Azure accepted the Return. Before the fix the Return ran
// on the request context, so the handler saw context.Canceled, recorded
// nothing and left the flag set; now the Return runs detached and reports the
// acceptance.
func TestRevokeAzure_RequestCanceledAfterReturn_RecordsAndRevokes(t *testing.T) {
	// NOT parallel: mutates the package-global revokeMarkRetryBackoffs, so a
	// regression fails fast instead of sleeping through the retries.
	orig := revokeMarkRetryBackoffs
	revokeMarkRetryBackoffs = []time.Duration{0, 0, 0}
	t.Cleanup(func() { revokeMarkRetryBackoffs = orig })

	pg, store := setupSubmitDB(t)
	ctx, cancelTest := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelTest()

	record := withinWindowRecord("/providers/Microsoft.Capacity/reservationOrders/order-1525/reservations/res-1525")
	record.AccountID = "acct-1525"
	record.Region = "eastus"
	record.ResourceType = "Standard_D2s_v3"
	require.NoError(t, store.SavePurchaseHistory(ctx, record))

	reqCtx, cancelReq := context.WithCancel(ctx)
	defer cancelReq()
	calc := &barrierCalcRefundClient{
		price:   &armreservations.Price{Amount: toPtr(10.0), CurrencyCode: toPtr("USD")},
		parties: 1,
		ready:   make(chan struct{}),
	}
	ret := &cancelAfterReturnClient{cancel: cancelReq}
	h := &Handler{config: store, azureRevokeFactory: &azureRevokeClientFactory{
		newCredential:            func() (azcore.TokenCredential, error) { return nil, nil },
		newCalculateRefundClient: func(azcore.TokenCredential) (azureCalculateRefundClient, error) { return calc, nil },
		newReturnClient:          func(azcore.TokenCredential) (azureReturnClient, error) { return ret, nil },
	}}
	body, err := json.Marshal(confirmedQuote(10, "USD"))
	require.NoError(t, err)
	req := &events.LambdaFunctionURLRequest{Body: string(body)}

	start := time.Now()
	result, err := h.loadAndRevokePurchaseHistory(reqCtx, req, revokeAdminSession(), record.PurchaseID)
	require.NoError(t, err)
	ret.assertDetached(t, start)
	require.ErrorIs(t, reqCtx.Err(), context.Canceled, "the request was canceled during the Return")
	res, ok := result.(*revokePurchaseResult)
	require.True(t, ok, "want revoked, got %T %+v", result, result)
	assert.Equal(t, "revoked", res.Status)

	var returnPostedAt, revokedAt *time.Time
	var inFlight bool
	require.NoError(t, pg.DB.Pool().QueryRow(ctx, `
		SELECT return_posted_at, revoked_at, revocation_in_flight
		  FROM purchase_history WHERE purchase_id = $1`, record.PurchaseID).
		Scan(&returnPostedAt, &revokedAt, &inFlight))
	assert.NotNil(t, returnPostedAt, "the accepted Return must be recorded")
	assert.NotNil(t, revokedAt, "the purchase must be marked revoked")
	assert.False(t, inFlight, "MarkPurchaseRevoked clears the in-flight flag")
}

// TestRevokeAzure_RequestCanceledDuringFailedReturn_ClearsFlag covers issue
// #1527's error path on real Postgres: the request is canceled while the
// Return is in flight and the Return then fails (Azure answered 503). The
// in-flight clear must still land, or the row stays flagged and unrevocable
// until the sweep releases it 2h later. Before the fix the clear ran on the
// canceled request context and pgx refused it.
func TestRevokeAzure_RequestCanceledDuringFailedReturn_ClearsFlag(t *testing.T) {
	pg, store := setupSubmitDB(t)
	ctx, cancelTest := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelTest()

	record := withinWindowRecord("/providers/Microsoft.Capacity/reservationOrders/order-1527/reservations/res-1527")
	record.AccountID = "acct-1527"
	record.Region = "eastus"
	record.ResourceType = "Standard_D2s_v3"
	require.NoError(t, store.SavePurchaseHistory(ctx, record))

	reqCtx, cancelReq := context.WithCancel(ctx)
	defer cancelReq()
	calc := &barrierCalcRefundClient{
		price:   &armreservations.Price{Amount: toPtr(10.0), CurrencyCode: toPtr("USD")},
		parties: 1,
		ready:   make(chan struct{}),
	}
	ret := &cancelAfterReturnClient{cancel: cancelReq, err: &azcore.ResponseError{StatusCode: 503}}
	h := &Handler{config: store, azureRevokeFactory: &azureRevokeClientFactory{
		newCredential:            func() (azcore.TokenCredential, error) { return nil, nil },
		newCalculateRefundClient: func(azcore.TokenCredential) (azureCalculateRefundClient, error) { return calc, nil },
		newReturnClient:          func(azcore.TokenCredential) (azureReturnClient, error) { return ret, nil },
	}}
	body, err := json.Marshal(confirmedQuote(10, "USD"))
	require.NoError(t, err)
	req := &events.LambdaFunctionURLRequest{Body: string(body)}

	start := time.Now()
	_, err = h.loadAndRevokePurchaseHistory(reqCtx, req, revokeAdminSession(), record.PurchaseID)
	require.Error(t, err, "a failed Return is an error")
	ret.assertDetached(t, start)
	require.ErrorIs(t, reqCtx.Err(), context.Canceled, "the request was canceled during the Return")

	var returnPostedAt, revokedAt *time.Time
	var inFlight bool
	require.NoError(t, pg.DB.Pool().QueryRow(ctx, `
		SELECT return_posted_at, revoked_at, revocation_in_flight
		  FROM purchase_history WHERE purchase_id = $1`, record.PurchaseID).
		Scan(&returnPostedAt, &revokedAt, &inFlight))
	assert.False(t, inFlight, "the failed Return's in-flight flag must be cleared")
	assert.Nil(t, returnPostedAt, "a failed Return must not be recorded")
	assert.Nil(t, revokedAt, "a failed Return must not revoke the purchase")
}
