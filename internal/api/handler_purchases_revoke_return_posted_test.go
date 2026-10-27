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
	mockStore.On("MarkRevocationReturnPosted", ctx, r.PurchaseID).Return(nil).Run(order.add("return")).Once()
	mockStore.On("MarkPurchaseRevoked", ctx, r.PurchaseID, mock.AnythingOfType("time.Time"),
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
	mockStore.On("MarkRevocationReturnPosted", ctx, r.PurchaseID).Return(dbDown).Twice()
	mockStore.On("MarkRevocationReturnPosted", ctx, r.PurchaseID).Return(nil).Once()
	mockStore.On("MarkPurchaseRevoked", ctx, r.PurchaseID, mock.AnythingOfType("time.Time"),
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
