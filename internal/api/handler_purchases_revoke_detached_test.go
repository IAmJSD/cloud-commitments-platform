package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil/lambdatimeout"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #1527: once the in-flight claim is held, the Azure Return runs
// detached from the request, the in-flight clear after a failed Return can
// still land, and the persist retries stay inside their budget. Stub Azure
// clients and a mock store; nothing here calls Azure.

// TestLambdaTimeoutCoversRevokeBudget keeps every shipped AWS Lambda timeout
// above the detached revoke budget: the Return and then either the writes
// recording it or the in-flight clear run back to back after the claim, and
// Lambda ends the invocation at its timeout whatever the context says (#913).
func TestLambdaTimeoutCoversRevokeBudget(t *testing.T) {
	lambdatimeout.AssertCovers(t, "../..", revokeReturnTimeout+revokePersistTimeout)
}

// The whole persist schedule (every attempt at its cap plus every backoff)
// must fit the persist budget, or the last retries are dead on arrival.
func TestRevokePersistScheduleFitsBudget(t *testing.T) {
	t.Parallel()
	schedule := time.Duration(len(revokeMarkRetryBackoffs)+1) * revokePersistAttemptTimeout
	for _, b := range revokeMarkRetryBackoffs {
		schedule += b
	}
	assert.LessOrEqual(t, schedule, revokePersistTimeout)
}

// A Return Azure refuses after the request was canceled under it must still
// clear this request's in-flight flag, on a live context: before the fix the
// clear ran on the dead request context, failed, and left the row flagged
// until the sweep released it 2h later. The refusal is a definite 4xx: since
// issue #1529 an ambiguous failure (a 5xx, say) keeps the flag on purpose.
func TestCallAzureReturn_RequestCanceledDuringFailedReturn_ClearsInFlight(t *testing.T) {
	t.Parallel()
	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	store := &ctxHonoringStore{MockConfigStore: mockStore}

	r := armReservationRecord()
	mockStore.On("ClearRevocationInFlight", mock.Anything, r.PurchaseID).Return(nil).Once()

	calcClient := &stubCalcRefundClientWithAmount{amount: 10.0, currency: "USD", sessID: "s-cancel-fail"}
	returnClient := &cancelAfterReturnClient{cancel: cancel, err: &azcore.ResponseError{StatusCode: 400, ErrorCode: "InvalidReservationID"}}
	h := &Handler{config: store}
	start := time.Now()
	_, err := h.callAzureReturn(reqCtx, calcClient, returnClient, r, "order-abc", "res-xyz", confirmedQuote(10, "USD"))
	require.Error(t, err)
	ce, isClientErr := IsClientError(err)
	require.True(t, isClientErr, "a 400 from Azure is a client error")
	assert.Equal(t, 400, ce.code)

	returnClient.assertDetached(t, start)
	require.ErrorIs(t, reqCtx.Err(), context.Canceled)
	writes := store.seenWrites()
	require.Len(t, writes, 1, "one in-flight clear")
	require.NoError(t, writes[0].err, "the in-flight clear ran under a canceled context")
	require.True(t, writes[0].bounded, "the in-flight clear must have its own bounded timeout")
	assert.LessOrEqual(t, writes[0].remaining, revokePersistTimeout)
	mockStore.AssertNotCalled(t, "MarkRevocationReturnPosted")
	mockStore.AssertNotCalled(t, "MarkPurchaseRevoked")
}

// A wait that would outlast the persist budget is not taken: before the fix
// the retries slept through their backoff whatever the budget said (about 43s
// worst case against a 30s budget), here an hour.
func TestPersistAzureRevocation_RetryWaitRespectsBudget(t *testing.T) {
	// NOT parallel: mutates the package-global revokeMarkRetryBackoffs.
	orig := revokeMarkRetryBackoffs
	revokeMarkRetryBackoffs = []time.Duration{time.Hour}
	t.Cleanup(func() { revokeMarkRetryBackoffs = orig })

	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	r := armReservationRecord()
	mockStore.On("MarkRevocationReturnPosted", mock.Anything, r.PurchaseID).Return(nil).Once()
	mockStore.On("MarkPurchaseRevoked", mock.Anything, r.PurchaseID, mock.AnythingOfType("time.Time"),
		"direct-api", "", mock.Anything, mock.Anything).Return(errors.New("db down")).Once()
	h := &Handler{config: mockStore}

	type outcome struct {
		result any
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := h.persistAzureRevocation(context.Background(), r, toPtr(10.0), "USD")
		done <- outcome{result, err}
	}()
	select {
	case got := <-done:
		require.NoError(t, got.err)
		pending, ok := got.result.(*revokeReconcilePendingResult)
		require.True(t, ok, "want RECONCILE_PENDING, got %T %+v", got.result, got.result)
		assert.Equal(t, "RECONCILE_PENDING", pending.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("persistAzureRevocation waited past its 30s budget instead of giving up")
	}
}

// A first persist attempt stuck on a hung connection is cut off at its own
// cap, leaving budget for the retry, which records the Return and revokes.
func TestPersistAzureRevocation_HungAttemptLeavesBudgetForRetry(t *testing.T) {
	// NOT parallel: mutates the package-global revokeMarkRetryBackoffs.
	orig := revokeMarkRetryBackoffs
	revokeMarkRetryBackoffs = []time.Duration{0, 0, 0}
	t.Cleanup(func() { revokeMarkRetryBackoffs = orig })

	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	r := armReservationRecord()
	var hungFor time.Duration
	mockStore.On("MarkRevocationReturnPosted", mock.Anything, r.PurchaseID).Run(func(args mock.Arguments) {
		start := time.Now()
		<-args.Get(0).(context.Context).Done()
		hungFor = time.Since(start)
	}).Return(context.DeadlineExceeded).Once()
	mockStore.On("MarkPurchaseRevoked", mock.Anything, r.PurchaseID, mock.AnythingOfType("time.Time"),
		"direct-api", "", mock.Anything, mock.Anything).Return(context.DeadlineExceeded).Once()
	mockStore.On("MarkRevocationReturnPosted", mock.Anything, r.PurchaseID).Return(nil).Once()
	mockStore.On("MarkPurchaseRevoked", mock.Anything, r.PurchaseID, mock.AnythingOfType("time.Time"),
		"direct-api", "", mock.Anything, mock.Anything).Return(nil).Once()
	h := &Handler{config: mockStore}

	result, err := h.persistAzureRevocation(context.Background(), r, toPtr(10.0), "USD")
	require.NoError(t, err)
	rr, ok := result.(*revokePurchaseResult)
	require.True(t, ok, "want revoked, got %T %+v", result, result)
	assert.Equal(t, "revoked", rr.Status)
	assert.Less(t, hungFor, revokePersistTimeout/2, "the hung attempt must be cut off at its own cap, not the whole budget")
}
