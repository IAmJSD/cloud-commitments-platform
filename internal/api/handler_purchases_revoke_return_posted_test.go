package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	armreservations "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/reservations/armreservations"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #1523: the finalize_revocations sweep only finalizes rows whose Return
// was recorded as accepted, so the handler must record it after Azure accepts
// the Return and before (or while retrying) MarkPurchaseRevoked. Stub Azure
// clients and a mock store; nothing here calls Azure.

// callOrder records the store calls in the order the handler makes them.
type callOrder struct {
	mu    sync.Mutex
	calls []string
}

func (o *callOrder) add(name string) func(mock.Arguments) {
	return func(mock.Arguments) {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.calls = append(o.calls, name)
	}
}

func (o *callOrder) get() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.calls...)
}

func TestCallAzureReturn_RecordsReturnBeforeMark(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })

	r := armReservationRecord()
	order := &callOrder{}
	mockStore.On("MarkRevocationReturnPosted", mock.Anything, r.PurchaseID).Return(nil).Run(order.add("return")).Once()
	mockStore.On("MarkPurchaseRevoked", mock.Anything, r.PurchaseID, mock.AnythingOfType("time.Time"),
		"direct-api", "", mock.Anything, mock.Anything).Return(nil).Run(order.add("mark")).Once()

	calcClient := &stubCalcRefundClientWithAmount{amount: 10.0, currency: "USD", sessID: "s-ok"}
	returnClient := &stubReturnClient{resp: armreservations.ReturnClientPostResponse{}}
	h := &Handler{config: mockStore}
	result, err := h.callAzureReturn(ctx, calcClient, returnClient, r, "order-abc", "res-xyz", confirmedQuote(10, "USD"))
	require.NoError(t, err)
	rr, ok := result.(*revokePurchaseResult)
	require.True(t, ok)
	assert.Equal(t, "revoked", rr.Status)
	assert.Equal(t, []string{"return", "mark"}, order.get(), "the accepted Return must be recorded before the mark")
}

// A Return Azure refused must not be recorded: the sweep would otherwise
// finalize a purchase that was never refunded.
func TestCallAzureReturn_ReturnErrorRecordsNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mockStore := new(MockConfigStore)

	r := armReservationRecord()
	calcClient := &stubCalcRefundClientWithAmount{amount: 10.0, currency: "USD", sessID: "s-fail"}
	returnClient := &stubReturnClient{err: errors.New("dial tcp: connection refused")}
	h := &Handler{config: mockStore}
	_, err := h.callAzureReturn(ctx, calcClient, returnClient, r, "order-abc", "res-xyz", confirmedQuote(10, "USD"))
	require.Error(t, err)
	mockStore.AssertNotCalled(t, "MarkRevocationReturnPosted")
	mockStore.AssertNotCalled(t, "MarkPurchaseRevoked")
}

// When the stamp and the mark both fail, every retry retries the stamp too,
// until it lands; after that only the mark is retried. If the stamp lands and
// the mark never does, the 207 is honest: the sweep will finalize the row.
func TestCallAzureReturn_RetriesReturnRecordUntilItLands(t *testing.T) {
	// NOT parallel: mutates the package-global revokeMarkRetryBackoffs.
	orig := revokeMarkRetryBackoffs
	revokeMarkRetryBackoffs = []time.Duration{0, 0, 0}
	t.Cleanup(func() { revokeMarkRetryBackoffs = orig })

	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })

	r := armReservationRecord()
	dbDown := errors.New("db down")
	mockStore.On("MarkRevocationReturnPosted", mock.Anything, r.PurchaseID).Return(dbDown).Twice()
	mockStore.On("MarkRevocationReturnPosted", mock.Anything, r.PurchaseID).Return(nil).Once()
	mockStore.On("MarkPurchaseRevoked", mock.Anything, r.PurchaseID, mock.AnythingOfType("time.Time"),
		"direct-api", "", mock.Anything, mock.Anything).Return(dbDown).Times(4)

	calcClient := &stubCalcRefundClientWithAmount{amount: 10.0, currency: "USD", sessID: "s-retry"}
	returnClient := &stubReturnClient{resp: armreservations.ReturnClientPostResponse{}}
	h := &Handler{config: mockStore}
	result, err := h.callAzureReturn(ctx, calcClient, returnClient, r, "order-abc", "res-xyz", confirmedQuote(10, "USD"))
	require.NoError(t, err)
	pending, ok := result.(*revokeReconcilePendingResult)
	require.True(t, ok)
	assert.Equal(t, "RECONCILE_PENDING", pending.Code)
	mockStore.AssertNumberOfCalls(t, "MarkRevocationReturnPosted", 3)
	mockStore.AssertNumberOfCalls(t, "MarkPurchaseRevoked", 4)
}

// Issue #1525: once Azure has accepted the Return, the request being canceled
// (client disconnect, an upstream timeout) must not stop the handler from
// recording it. The writes before the Return must still honor cancellation.

// writeCtxState is the state of a write's context when the write ran.
type writeCtxState struct {
	err       error
	bounded   bool
	remaining time.Duration
}

// ctxHonoringStore fails the revoke writes with ctx.Err() once ctx is done, as
// pgx does, and records the state of the context each write ran under.
type ctxHonoringStore struct {
	*MockConfigStore
	mu     sync.Mutex
	writes []writeCtxState
}

func (s *ctxHonoringStore) seen(ctx context.Context) error {
	deadline, bounded := ctx.Deadline()
	s.mu.Lock()
	s.writes = append(s.writes, writeCtxState{err: ctx.Err(), bounded: bounded, remaining: time.Until(deadline)})
	s.mu.Unlock()
	return ctx.Err()
}

func (s *ctxHonoringStore) seenWrites() []writeCtxState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]writeCtxState(nil), s.writes...)
}

func (s *ctxHonoringStore) FlipPurchaseRevocationInFlight(ctx context.Context, purchaseID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.MockConfigStore.FlipPurchaseRevocationInFlight(ctx, purchaseID)
}

func (s *ctxHonoringStore) MarkRevocationReturnPosted(ctx context.Context, purchaseID string) error {
	if err := s.seen(ctx); err != nil {
		return err
	}
	return s.MockConfigStore.MarkRevocationReturnPosted(ctx, purchaseID)
}

func (s *ctxHonoringStore) MarkPurchaseRevoked(ctx context.Context, purchaseID string, revokedAt time.Time, revokedVia, supportCaseID string, calcRefundAmount *float64, calcRefundCurrency string) error {
	if err := s.seen(ctx); err != nil {
		return err
	}
	return s.MockConfigStore.MarkPurchaseRevoked(ctx, purchaseID, revokedAt, revokedVia, supportCaseID, calcRefundAmount, calcRefundCurrency)
}

// cancelAfterReturnClient accepts the Return, then cancels the request, as a
// client disconnecting right after Azure answered would. It records whether
// the Return itself ran under the request's (cancelable) context.
type cancelAfterReturnClient struct {
	cancel        context.CancelFunc
	calls         int
	sawRequestCtx bool
}

func (c *cancelAfterReturnClient) Post(ctx context.Context, _ string, _ armreservations.RefundRequest, _ *armreservations.ReturnClientPostOptions) (armreservations.ReturnClientPostResponse, error) {
	c.calls++
	before := ctx.Err()
	c.cancel()
	c.sawRequestCtx = before == nil && ctx.Err() != nil
	return armreservations.ReturnClientPostResponse{}, nil
}

func TestCallAzureReturn_RequestCanceledAfterReturn_StillRecordsAndMarks(t *testing.T) {
	// NOT parallel: mutates the package-global revokeMarkRetryBackoffs, so a
	// regression fails in milliseconds instead of sleeping through the retries.
	orig := revokeMarkRetryBackoffs
	revokeMarkRetryBackoffs = []time.Duration{0, 0, 0}
	t.Cleanup(func() { revokeMarkRetryBackoffs = orig })

	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	store := &ctxHonoringStore{MockConfigStore: mockStore}

	r := armReservationRecord()
	mockStore.On("MarkRevocationReturnPosted", mock.Anything, r.PurchaseID).Return(nil).Once()
	mockStore.On("MarkPurchaseRevoked", mock.Anything, r.PurchaseID, mock.AnythingOfType("time.Time"),
		"direct-api", "", mock.Anything, mock.Anything).Return(nil).Once()

	calcClient := &stubCalcRefundClientWithAmount{amount: 10.0, currency: "USD", sessID: "s-cancel"}
	returnClient := &cancelAfterReturnClient{cancel: cancel}
	h := &Handler{config: store}
	result, err := h.callAzureReturn(reqCtx, calcClient, returnClient, r, "order-abc", "res-xyz", confirmedQuote(10, "USD"))
	require.NoError(t, err)

	require.Equal(t, 1, returnClient.calls)
	assert.True(t, returnClient.sawRequestCtx, "the Return itself must run under the request's context")
	require.ErrorIs(t, reqCtx.Err(), context.Canceled)
	rr, ok := result.(*revokePurchaseResult)
	require.True(t, ok, "a canceled request must still record the accepted Return and revoke, got %T %+v", result, result)
	assert.Equal(t, "revoked", rr.Status)

	writes := store.seenWrites()
	require.Len(t, writes, 2, "one record, one mark")
	for i, w := range writes {
		require.NoError(t, w.err, "write %d ran under a canceled context", i)
		require.True(t, w.bounded, "write %d must have its own bounded timeout", i)
		assert.LessOrEqual(t, w.remaining, revokePersistTimeout, "write %d", i)
	}
}

// cancelingCalcRefundClient quotes a refund, then cancels the request before
// the handler gets to the claim and the Return.
type cancelingCalcRefundClient struct {
	stubCalcRefundClientWithAmount
	cancel context.CancelFunc
}

func (c *cancelingCalcRefundClient) Post(ctx context.Context, orderID string, body armreservations.CalculateRefundRequest, opts *armreservations.CalculateRefundClientPostOptions) (armreservations.CalculateRefundClientPostResponse, error) {
	resp, err := c.stubCalcRefundClientWithAmount.Post(ctx, orderID, body, opts)
	c.cancel()
	return resp, err
}

// A request canceled before the Return must stop there: no write before the
// Return ignores cancellation, so no claim lands and no Return is posted.
func TestCallAzureReturn_RequestCanceledBeforeReturn_PostsNothing(t *testing.T) {
	t.Parallel()
	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mockStore := new(MockConfigStore)
	store := &ctxHonoringStore{MockConfigStore: mockStore}

	r := armReservationRecord()
	calcClient := &cancelingCalcRefundClient{
		stubCalcRefundClientWithAmount: stubCalcRefundClientWithAmount{amount: 10.0, currency: "USD", sessID: "s-early"},
		cancel:                         cancel,
	}
	returnClient := &stubReturnClient{}
	h := &Handler{config: store}
	_, err := h.callAzureReturn(reqCtx, calcClient, returnClient, r, "order-abc", "res-xyz", confirmedQuote(10, "USD"))
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, returnClient.calls, "no Return may be posted for a canceled request")
	assert.Empty(t, store.seenWrites())
	mockStore.AssertNotCalled(t, "FlipPurchaseRevocationInFlight")
	mockStore.AssertNotCalled(t, "MarkRevocationReturnPosted")
	mockStore.AssertNotCalled(t, "MarkPurchaseRevoked")
}
