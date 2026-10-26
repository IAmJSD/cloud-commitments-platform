package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	armreservations "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/reservations/armreservations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// countingCalcRefundClient returns a fixed CalculateRefund response and counts
// calls, so tests can assert whether Azure was consulted at all.
type countingCalcRefundClient struct {
	price *armreservations.Price
	calls int
}

func (s *countingCalcRefundClient) Post(_ context.Context, _ string, _ armreservations.CalculateRefundRequest, _ *armreservations.CalculateRefundClientPostOptions) (armreservations.CalculateRefundClientPostResponse, error) {
	s.calls++
	return armreservations.CalculateRefundClientPostResponse{
		CalculateRefundResponse: armreservations.CalculateRefundResponse{
			Properties: &armreservations.RefundResponseProperties{
				SessionID:           toPtr("s-guard"),
				BillingRefundAmount: s.price,
			},
		},
	}, nil
}

// TestRevokePurchase_AzureRefundGuard drives POST /api/purchases/{id}/revoke
// end to end and asserts the refund-divergence guard is mandatory and
// currency-aware (platform#96): the Return call, which issues an irreversible
// refund, must only happen when the confirmed amount AND currency match
// Azure's current quote.
func TestRevokePurchase_AzureRefundGuard(t *testing.T) {
	t.Parallel()

	usd := func(amount float64) *armreservations.Price {
		return &armreservations.Price{Amount: toPtr(amount), CurrencyCode: toPtr("USD")}
	}

	tests := []struct {
		name       string
		body       string
		quote      *armreservations.Price
		wantCode   int // 0 means success
		wantMsg    string
		wantCalc   int
		wantReturn int
	}{
		{
			name:     "missing body rejected before Azure is called",
			body:     "",
			quote:    usd(100),
			wantCode: 400,
			wantMsg:  "expected_refund_amount",
		},
		{
			name:     "missing expected currency rejected",
			body:     `{"expected_refund_amount": 100}`,
			quote:    usd(100),
			wantCode: 400,
			wantMsg:  "expected_refund_currency",
		},
		{
			name:     "missing expected amount rejected",
			body:     `{"expected_refund_currency": "USD"}`,
			quote:    usd(100),
			wantCode: 400,
			wantMsg:  "expected_refund_amount",
		},
		{
			name:     "Azure quote without amount fails closed",
			body:     `{"expected_refund_amount": 100, "expected_refund_currency": "USD"}`,
			quote:    nil,
			wantCode: 422,
			wantMsg:  "no refund quote",
			wantCalc: 1,
		},
		{
			name:     "Azure quote without currency fails closed",
			body:     `{"expected_refund_amount": 100, "expected_refund_currency": "USD"}`,
			quote:    &armreservations.Price{Amount: toPtr(100.0)},
			wantCode: 422,
			wantMsg:  "no refund quote",
			wantCalc: 1,
		},
		{
			name:     "currency mismatch at equal amount rejected",
			body:     `{"expected_refund_amount": 4200, "expected_refund_currency": "EUR"}`,
			quote:    usd(4200),
			wantCode: 422,
			wantMsg:  "refund currency diverged",
			wantCalc: 1,
		},
		{
			name:     "amount divergence rejected",
			body:     `{"expected_refund_amount": 4200, "expected_refund_currency": "USD"}`,
			quote:    usd(1100),
			wantCode: 422,
			wantMsg:  "refund amount diverged",
			wantCalc: 1,
		},
		{
			name:       "matching amount and currency revokes",
			body:       `{"expected_refund_amount": 100, "expected_refund_currency": "usd"}`,
			quote:      usd(100),
			wantCalc:   1,
			wantReturn: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			mockStore := new(MockConfigStore)
			mockAuth := new(MockAuthService)
			t.Cleanup(func() {
				mockStore.AssertExpectations(t)
				mockAuth.AssertExpectations(t)
			})

			r := armReservationRecord()
			mockAuth.On("ValidateSession", ctx, "tok").Return(revokeAdminSession(), nil)
			mockStore.On("GetExecutionByID", ctx, r.PurchaseID).Return(nil, fmt.Errorf("%w: execution %s", config.ErrNotFound, r.PurchaseID))
			mockStore.On("GetPurchaseHistoryByPurchaseID", ctx, r.PurchaseID).Return(r, nil)
			mockStore.On("FlipPurchaseRevocationInFlight", ctx, r.PurchaseID).Return(nil).Maybe()
			mockStore.On("MarkPurchaseRevoked", ctx, r.PurchaseID, mock.AnythingOfType("time.Time"), "direct-api", "", mock.Anything, mock.Anything).Return(nil).Maybe()

			calcClient := &countingCalcRefundClient{price: tc.quote}
			returnClient := &stubReturnClient{}
			h := &Handler{
				config: mockStore,
				auth:   mockAuth,
				azureRevokeFactory: &azureRevokeClientFactory{
					newCredential: func() (azcore.TokenCredential, error) { return nil, nil },
					newCalculateRefundClient: func(azcore.TokenCredential) (azureCalculateRefundClient, error) {
						return calcClient, nil
					},
					newReturnClient: func(azcore.TokenCredential) (azureReturnClient, error) {
						return returnClient, nil
					},
				},
			}

			req := &events.LambdaFunctionURLRequest{
				Headers: map[string]string{"Authorization": "Bearer tok"},
				Body:    tc.body,
			}
			result, err := h.revokePurchase(ctx, req, r.PurchaseID)

			if tc.wantCode == 0 {
				require.NoError(t, err)
				m, ok := result.(*revokePurchaseResult)
				require.True(t, ok)
				assert.Equal(t, "revoked", m.Status)
			} else {
				require.Error(t, err)
				ce, ok := IsClientError(err)
				require.True(t, ok, "want ClientError, got %v", err)
				assert.Equal(t, tc.wantCode, ce.code)
				assert.Contains(t, ce.message, tc.wantMsg)
			}
			assert.Equal(t, tc.wantCalc, calcClient.calls, "CalculateRefund calls")
			assert.Equal(t, tc.wantReturn, returnClient.calls, "Return (refund) calls")
		})
	}
}

func confirmedQuote(amount float64, currency string) revokeConfirmBody {
	return revokeConfirmBody{ExpectedRefundAmount: &amount, ExpectedRefundCurrency: currency}
}

// TestCallAzureReturn_FlipFindsRowRevoked covers issue #316: the in-flight
// flip is now a compare-and-set on revoked_at IS NULL. When it reports the
// row already revoked, a concurrent request finished the revocation after
// this one loaded the row, so no second Return is posted and the answer is
// already_revoked. Any other flip failure stays best-effort.
func TestCallAzureReturn_FlipFindsRowRevoked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	quote := &armreservations.Price{Amount: toPtr(10.0), CurrencyCode: toPtr("USD")}

	t.Run("already revoked: no Return, already_revoked", func(t *testing.T) {
		t.Parallel()
		mockStore := new(MockConfigStore)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		r := armReservationRecord()
		revokedAt := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
		mockStore.On("FlipPurchaseRevocationInFlight", ctx, r.PurchaseID).
			Return(fmt.Errorf("FlipPurchaseRevocationInFlight: purchase_id %q: %w", r.PurchaseID, config.ErrPurchaseAlreadyRevoked))
		current := *r
		current.RevokedAt = &revokedAt
		current.RevokedVia = "direct-api"
		mockStore.On("GetPurchaseHistoryByPurchaseID", ctx, r.PurchaseID).Return(&current, nil)
		returnClient := &stubReturnClient{}

		h := &Handler{config: mockStore}
		result, err := h.callAzureReturn(ctx, &countingCalcRefundClient{price: quote}, returnClient, r, "order-abc", "res-xyz", confirmedQuote(10, "USD"))
		require.NoError(t, err)
		assert.Zero(t, returnClient.calls, "a revoked row must not get a second Return")
		res, ok := result.(*revokePurchaseResult)
		require.True(t, ok, "got %T", result)
		assert.Equal(t, "already_revoked", res.Status)
		assert.Equal(t, revokedAt.Format(time.RFC3339), res.RevokedAt)
		assert.Equal(t, "direct-api", res.RevokedVia)
		mockStore.AssertNotCalled(t, "MarkPurchaseRevoked", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("other flip error: still returns and marks", func(t *testing.T) {
		t.Parallel()
		mockStore := new(MockConfigStore)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		r := armReservationRecord()
		mockStore.On("FlipPurchaseRevocationInFlight", ctx, r.PurchaseID).Return(fmt.Errorf("FlipPurchaseRevocationInFlight: connection reset"))
		mockStore.On("MarkPurchaseRevoked", ctx, r.PurchaseID, mock.AnythingOfType("time.Time"), "direct-api", "", mock.Anything, mock.Anything).Return(nil)
		returnClient := &stubReturnClient{}

		h := &Handler{config: mockStore}
		result, err := h.callAzureReturn(ctx, &countingCalcRefundClient{price: quote}, returnClient, r, "order-abc", "res-xyz", confirmedQuote(10, "USD"))
		require.NoError(t, err)
		assert.Equal(t, 1, returnClient.calls)
		res, ok := result.(*revokePurchaseResult)
		require.True(t, ok, "got %T", result)
		assert.Equal(t, "revoked", res.Status)
	})
}
