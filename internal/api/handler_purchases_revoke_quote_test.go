package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/reservations/armreservations"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #447: the quote endpoint must not fabricate a 0 / empty refund when
// Azure omits the amount or currency; it must fail with a 422 instead.

func quoteResponse(price *armreservations.Price) armreservations.CalculateRefundClientPostResponse {
	sessionID := "s1"
	return armreservations.CalculateRefundClientPostResponse{
		CalculateRefundResponse: armreservations.CalculateRefundResponse{
			Properties: &armreservations.RefundResponseProperties{
				SessionID:           &sessionID,
				BillingRefundAmount: price,
			},
		},
	}
}

func quoteHandlerRequest(purchaseID string) *events.LambdaFunctionURLRequest {
	return &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer tok"},
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{
				Method: "GET",
				Path:   "/api/purchases/" + purchaseID + "/revoke/calculate",
			},
		},
	}
}

func TestHandleRequest_RevokeCalculate_AzureQuote(t *testing.T) {
	amount := 75.25
	currency := "USD"
	empty := ""

	cases := []struct {
		name       string
		resp       armreservations.CalculateRefundClientPostResponse
		wantStatus int
		wantErr    string
	}{
		{
			name:       "complete quote",
			resp:       quoteResponse(&armreservations.Price{Amount: &amount, CurrencyCode: &currency}),
			wantStatus: 200,
		},
		{
			name:       "zero amount is a real quote",
			resp:       quoteResponse(&armreservations.Price{Amount: new(float64), CurrencyCode: &currency}),
			wantStatus: 200,
		},
		{
			name:       "no billing refund amount",
			resp:       quoteResponse(nil),
			wantStatus: 422,
			wantErr:    "no refund amount or currency",
		},
		{
			name:       "no properties",
			resp:       armreservations.CalculateRefundClientPostResponse{},
			wantStatus: 422,
			wantErr:    "no refund amount or currency",
		},
		{
			name:       "amount missing",
			resp:       quoteResponse(&armreservations.Price{CurrencyCode: &currency}),
			wantStatus: 422,
			wantErr:    "no refund amount or currency",
		},
		{
			name:       "currency missing",
			resp:       quoteResponse(&armreservations.Price{Amount: &amount}),
			wantStatus: 422,
			wantErr:    "no refund amount or currency",
		},
		{
			name:       "currency blank",
			resp:       quoteResponse(&armreservations.Price{Amount: &amount, CurrencyCode: &empty}),
			wantStatus: 422,
			wantErr:    "no refund amount or currency",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			mockStore := new(MockConfigStore)
			t.Cleanup(func() { mockStore.AssertExpectations(t) })
			mockAuth := new(MockAuthService)
			t.Cleanup(func() { mockAuth.AssertExpectations(t) })
			mockAuth.On("ValidateSession", mock.Anything, "tok").Return(revokeAdminSession(), nil)
			r := armReservationRecord()
			mockStore.On("GetPurchaseHistoryByPurchaseID", mock.Anything, r.PurchaseID).Return(r, nil)

			h := &Handler{
				config: mockStore,
				auth:   mockAuth,
				azureRevokeFactory: &azureRevokeClientFactory{
					newCredential: func() (azcore.TokenCredential, error) { return nil, nil },
					newCalculateRefundClient: func(azcore.TokenCredential) (azureCalculateRefundClient, error) {
						return &stubCalcRefundClient{resp: tc.resp}, nil
					},
					newReturnClient: func(azcore.TokenCredential) (azureReturnClient, error) {
						return &stubReturnClient{}, nil
					},
				},
			}

			resp, err := h.HandleRequest(ctx, quoteHandlerRequest(r.PurchaseID))
			require.NoError(t, err)
			require.Equal(t, tc.wantStatus, resp.StatusCode, resp.Body)
			if tc.wantErr != "" {
				assert.Contains(t, resp.Body, tc.wantErr)
				assert.NotContains(t, resp.Body, "refund_amount")
				return
			}
			var body map[string]any
			require.NoError(t, json.Unmarshal([]byte(resp.Body), &body))
			assert.Equal(t, "USD", body["refund_currency"])
		})
	}
}
