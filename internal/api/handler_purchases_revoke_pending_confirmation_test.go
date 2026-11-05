package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	armreservations "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/reservations/armreservations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issues #1522 and #1529: a revoke that cannot prove Azure accepted the Return
// answers REVOCATION_PENDING_CONFIRMATION with no azure_returned, and only a
// recorded return_posted_at stamp (or this request's own accepted Return)
// lets a response say azure_returned:true. The tests read the response the
// way a client does, as JSON.

const pendingConfirmationMessage = "A refund for this purchase is being processed. Check back in a few minutes; don't start another revoke."

func resultJSON(t *testing.T, result any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(result)
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	return body
}

func assertPendingConfirmation(t *testing.T, result any) {
	t.Helper()
	body := resultJSON(t, result)
	assert.Equal(t, "REVOCATION_PENDING_CONFIRMATION", body["code"])
	assert.Equal(t, pendingConfirmationMessage, body["message"])
	assert.EqualValues(t, 300, body["retry_after_seconds"])
	assert.NotContains(t, body, "azure_returned", "an unproven outcome must not say whether Azure returned")
}

func assertReconcilePendingReturned(t *testing.T, result any) {
	t.Helper()
	body := resultJSON(t, result)
	assert.Equal(t, "RECONCILE_PENDING", body["code"])
	assert.Equal(t, true, body["azure_returned"])
	assert.NotContains(t, body, "retry_after_seconds")
}

func inFlightRow(stamp *time.Time) *config.PurchaseHistoryRecord {
	r := armReservationRecord()
	r.RevocationInFlight = true
	r.RevokedAt = nil
	r.ReturnPostedAt = stamp
	return r
}

func upFrontResult(t *testing.T, row *config.PurchaseHistoryRecord) any {
	t.Helper()
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", ctx, "tok").Return(revokeAdminSession(), nil)
	mockStore.On("GetExecutionByID", ctx, row.PurchaseID).Return(nil, fmt.Errorf("%w: execution %s", config.ErrNotFound, row.PurchaseID))
	mockStore.On("GetPurchaseHistoryByPurchaseID", ctx, row.PurchaseID).Return(row, nil)
	h := &Handler{config: mockStore, auth: mockAuth}
	result, err := h.revokePurchase(ctx, sessionReq("tok"), row.PurchaseID)
	require.NoError(t, err)
	return result
}

func TestRevokePurchase_InFlightRow_StampUnset_PendingConfirmation(t *testing.T) {
	t.Parallel()
	assertPendingConfirmation(t, upFrontResult(t, inFlightRow(nil)))
}

func TestRevokePurchase_InFlightRow_StampSet_ReconcilePending(t *testing.T) {
	t.Parallel()
	stamp := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	assertReconcilePendingReturned(t, upFrontResult(t, inFlightRow(&stamp)))
}

// Lost claim: the flip says another request holds it, so the row is re-read
// to see whether that request's Return has been recorded.
func TestCallAzureReturn_LostClaim_ReReadsStamp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	quote := &armreservations.Price{Amount: toPtr(10.0), CurrencyCode: toPtr("USD")}
	stamp := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		reread  func(r *config.PurchaseHistoryRecord) (*config.PurchaseHistoryRecord, error)
		assertR func(t *testing.T, result any)
	}{
		"stamp unset": {
			reread: func(r *config.PurchaseHistoryRecord) (*config.PurchaseHistoryRecord, error) {
				return inFlightRow(nil), nil
			},
			assertR: assertPendingConfirmation,
		},
		"stamp set": {
			reread: func(r *config.PurchaseHistoryRecord) (*config.PurchaseHistoryRecord, error) {
				return inFlightRow(&stamp), nil
			},
			assertR: assertReconcilePendingReturned,
		},
		"re-read fails": {
			reread: func(r *config.PurchaseHistoryRecord) (*config.PurchaseHistoryRecord, error) {
				return nil, errors.New("connection reset")
			},
			assertR: assertPendingConfirmation,
		},
		"re-read finds no row": {
			reread: func(r *config.PurchaseHistoryRecord) (*config.PurchaseHistoryRecord, error) {
				return nil, nil
			},
			assertR: assertPendingConfirmation,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mockStore := new(MockConfigStore)
			t.Cleanup(func() { mockStore.AssertExpectations(t) })
			r := armReservationRecord()
			mockStore.On("FlipPurchaseRevocationInFlight", ctx, r.PurchaseID).
				Return(fmt.Errorf("FlipPurchaseRevocationInFlight: purchase_id %q: %w", r.PurchaseID, config.ErrRevocationInFlight))
			current, rereadErr := tc.reread(r)
			mockStore.On("GetPurchaseHistoryByPurchaseID", ctx, r.PurchaseID).Return(current, rereadErr)
			returnClient := &stubReturnClient{}

			h := &Handler{config: mockStore}
			result, err := h.callAzureReturn(ctx, &countingCalcRefundClient{price: quote}, returnClient, r, "order-abc", "res-xyz", confirmedQuote(10, "USD"))
			require.NoError(t, err)
			assert.Zero(t, returnClient.calls, "the request that loses the claim must not post a Return")
			tc.assertR(t, result)
			mockStore.AssertNotCalled(t, "ClearRevocationInFlight", mock.Anything, mock.Anything)
		})
	}
}

// The claim holder may finish between this request's flip and its re-read:
// the row is then revoked, and the honest answer is already_revoked.
func TestCallAzureReturn_LostClaim_ReReadShowsRevoked_AlreadyRevoked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	quote := &armreservations.Price{Amount: toPtr(10.0), CurrencyCode: toPtr("USD")}
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	r := armReservationRecord()
	mockStore.On("FlipPurchaseRevocationInFlight", ctx, r.PurchaseID).
		Return(fmt.Errorf("FlipPurchaseRevocationInFlight: purchase_id %q: %w", r.PurchaseID, config.ErrRevocationInFlight))
	revokedAt := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	current := inFlightRow(nil)
	current.RevokedAt = &revokedAt
	current.RevokedVia = "direct-api"
	mockStore.On("GetPurchaseHistoryByPurchaseID", ctx, r.PurchaseID).Return(current, nil)
	returnClient := &stubReturnClient{}

	h := &Handler{config: mockStore}
	result, err := h.callAzureReturn(ctx, &countingCalcRefundClient{price: quote}, returnClient, r, "order-abc", "res-xyz", confirmedQuote(10, "USD"))
	require.NoError(t, err)
	assert.Zero(t, returnClient.calls)
	body := resultJSON(t, result)
	assert.Equal(t, "already_revoked", body["status"])
	assert.Equal(t, revokedAt.Format(time.RFC3339), body["revoked_at"])
	assert.Equal(t, "direct-api", body["revoked_via"])
}

// Issue #1529: a Return that failed without a definite answer answers the
// shared pending-confirmation response and keeps the flag.
func TestCallAzureReturn_Uncertain_AnswersPendingConfirmation(t *testing.T) {
	t.Parallel()
	mockStore := new(MockConfigStore)
	calcClient := &stubCalcRefundClientWithAmount{amount: 10.0, currency: "USD", sessID: "s-uncertain"}
	returnClient := &stubReturnClient{err: &azcore.ResponseError{StatusCode: 504, ErrorCode: "GatewayTimeout"}}
	h := &Handler{config: mockStore}

	result, err := h.callAzureReturn(context.Background(), calcClient, returnClient, armReservationRecord(), "order-abc", "res-xyz", confirmedQuote(10, "USD"))
	require.NoError(t, err)
	assertPendingConfirmation(t, result)
	assert.Equal(t, 1, returnClient.calls, "exactly one Return")
	mockStore.AssertCalled(t, "FlipPurchaseRevocationInFlight")
	mockStore.AssertNotCalled(t, "ClearRevocationInFlight")
	mockStore.AssertNotCalled(t, "MarkRevocationReturnPosted")
	mockStore.AssertNotCalled(t, "MarkPurchaseRevoked")
}

// No pending-path response may claim azure_returned:true without the stamp,
// except this request's own accepted Return whose DB mark failed.
func TestRevokePending_NeverClaimsReturnedWithoutStamp(t *testing.T) {
	t.Parallel()
	stamp := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		result       any
		wantReturned bool
	}{
		"up-front, no stamp": {upFrontResult(t, inFlightRow(nil)), false},
		"up-front, stamp":    {upFrontResult(t, inFlightRow(&stamp)), true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body := resultJSON(t, tc.result)
			returned, present := body["azure_returned"]
			if !tc.wantReturned {
				assert.False(t, present, "azure_returned must be absent without the stamp")
				return
			}
			assert.Equal(t, true, returned)
		})
	}
}
