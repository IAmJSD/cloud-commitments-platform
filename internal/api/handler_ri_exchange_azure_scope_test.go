package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/reservations/armreservations"
	azurecompute "github.com/LeanerCloud/cloud-commitments-go/providers/azure/services/compute"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/require"
)

type azureScopeExchangeClient struct {
	*azurecompute.Client
	owned     []azurecompute.ExchangeableReservation
	priced    []azurecompute.ExchangeableReservation
	listCalls int
}

func (c *azureScopeExchangeClient) ListExchangeableReservations(context.Context) ([]azurecompute.ExchangeableReservation, error) {
	c.listCalls++
	return c.owned, nil
}

func (c *azureScopeExchangeClient) CalculateExchange(ctx context.Context, sources []azurecompute.ExchangeableReservation, targets []azurecompute.ExchangeTarget) (*azurecompute.ExchangePreview, []azurecompute.CompatibleOffering, error) {
	c.priced = sources
	return c.Client.CalculateExchange(ctx, sources, targets)
}

func azureScopeRequest(t *testing.T, sources []AzureExchangeSourceBody) *events.LambdaFunctionURLRequest {
	t.Helper()
	var body AzureExecuteExchangeRequestBody
	require.NoError(t, json.Unmarshal([]byte(validAzureExecuteBody), &body))
	body.Sources = sources
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	return &events.LambdaFunctionURLRequest{Headers: map[string]string{"authorization": "Bearer tok"}, Body: string(encoded)}
}

func azureScopeHandler(t *testing.T, execute bool, client azureExchangeClient) *Handler {
	t.Helper()
	if execute {
		return newAzureExecuteSourceGateHandler(t, client)
	}
	return &Handler{
		auth:                 scopedAzureAuth(t, "view", "purchases", nil),
		azureExchangeFactory: func(string) azureExchangeClient { return client },
	}
}

func TestAzureExchangeScopePreserved(t *testing.T) {
	for _, execute := range []bool{false, true} {
		for _, scope := range []armreservations.AppliedScopeType{armreservations.AppliedScopeTypeShared, armreservations.AppliedScopeTypeSingle} {
			name := "preview/" + string(scope)
			if execute {
				name = "execute/" + string(scope)
			}
			t.Run(name, func(t *testing.T) {
				owned := azurecompute.ExchangeableReservation{
					ReservationID: "res-1", ReservationOrderID: "order-1",
					BillingScopeID: "/subscriptions/sub-1", AppliedScopeType: scope,
					SKU: "Standard_D2s_v3", Region: "eastus", Quantity: 4,
				}
				if scope == armreservations.AppliedScopeTypeSingle {
					owned.AppliedScopes = []string{"/subscriptions/discount-sub/resourceGroups/discount-rg"}
				}
				client := &azureScopeExchangeClient{Client: new(azurecompute.Client), owned: []azurecompute.ExchangeableReservation{owned}}
				calculateCalls, executeCalls := 0, 0
				client.SetCalculateExchangeCaller(func(_ context.Context, request armreservations.CalculateExchangeRequest) (armreservations.CalculateExchangeOperationResultResponse, error) {
					calculateCalls++
					props := request.Properties
					require.NotNil(t, props)
					require.Len(t, props.ReservationsToExchange, 1)
					require.Equal(t, owned.ReservationID, *props.ReservationsToExchange[0].ReservationID)
					require.Equal(t, int32(2), *props.ReservationsToExchange[0].Quantity)
					require.Len(t, props.ReservationsToPurchase, 1)
					target := props.ReservationsToPurchase[0].Properties
					require.Equal(t, scope, *target.AppliedScopeType)
					require.Equal(t, "/subscriptions/sub-1", *target.BillingScopeID)
					if scope == armreservations.AppliedScopeTypeSingle {
						require.Len(t, target.AppliedScopes, 1)
						require.Equal(t, owned.AppliedScopes[0], *target.AppliedScopes[0])
					} else {
						require.Nil(t, target.AppliedScopes)
					}
					return armreservations.CalculateExchangeOperationResultResponse{Properties: &armreservations.CalculateExchangeResponseProperties{
						SessionID: toPtr("scope-session"), NetPayable: &armreservations.Price{Amount: toPtr(10.0), CurrencyCode: toPtr("USD")},
					}}, nil
				})
				client.SetDoExchangeCaller(func(_ context.Context, sessionID string) (armreservations.ExchangeOperationResultResponse, error) {
					executeCalls++
					require.Equal(t, "scope-session", sessionID)
					return armreservations.ExchangeOperationResultResponse{Properties: &armreservations.ExchangeResponseProperties{}, Status: toPtr(armreservations.ExchangeOperationResultStatusSucceeded)}, nil
				})
				h := azureScopeHandler(t, execute, client)
				req := azureScopeRequest(t, []AzureExchangeSourceBody{{ReservationID: strings.ToUpper(owned.ReservationID), Quantity: 2}})
				var err error
				if execute {
					_, err = h.executeAzureExchange(context.Background(), req)
				} else {
					_, err = h.getAzureCompatibleOfferings(context.Background(), req)
				}
				require.NoError(t, err)
				require.Equal(t, 1, calculateCalls)
				require.Equal(t, 1, client.listCalls)
				want := owned
				want.Quantity = 2
				require.Equal(t, []azurecompute.ExchangeableReservation{want}, client.priced)
				require.Equal(t, []azurecompute.ExchangeableReservation{owned}, client.owned)
				if execute {
					require.Equal(t, 1, executeCalls)
				} else {
					require.Zero(t, executeCalls)
				}
			})
		}
	}
}

func TestAzureExchangeScopeRefused(t *testing.T) {
	base := azurecompute.ExchangeableReservation{ReservationID: "res-1", BillingScopeID: "/subscriptions/sub-1", AppliedScopeType: armreservations.AppliedScopeTypeShared, Region: "eastus", Quantity: 4}
	missing := base
	missing.AppliedScopeType = ""
	unsupported := base
	unsupported.AppliedScopeType = armreservations.AppliedScopeType("ManagementGroup")
	singleMissing := base
	singleMissing.AppliedScopeType = armreservations.AppliedScopeTypeSingle
	mixed := base
	mixed.ReservationID = "res-2"
	mixed.AppliedScopeType = armreservations.AppliedScopeTypeSingle
	mixed.AppliedScopes = []string{"/subscriptions/sub-1"}
	foreign := base
	foreign.BillingScopeID = "/subscriptions/foreign"
	for _, execute := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			owned   []azurecompute.ExchangeableReservation
			foreign bool
		}{
			{name: "missing", owned: []azurecompute.ExchangeableReservation{missing}},
			{name: "unsupported", owned: []azurecompute.ExchangeableReservation{unsupported}},
			{name: "single_missing_scopes", owned: []azurecompute.ExchangeableReservation{singleMissing}},
			{name: "mixed", owned: []azurecompute.ExchangeableReservation{base, mixed}},
			{name: "foreign_billing", owned: []azurecompute.ExchangeableReservation{foreign}, foreign: true},
		} {
			name := "preview/" + tc.name
			if execute {
				name = "execute/" + tc.name
			}
			t.Run(name, func(t *testing.T) {
				client := &azureScopeExchangeClient{Client: new(azurecompute.Client), owned: tc.owned}
				client.SetCalculateExchangeCaller(func(context.Context, armreservations.CalculateExchangeRequest) (armreservations.CalculateExchangeOperationResultResponse, error) {
					t.Fatal("a rejected source must not reach the calculate SDK caller")
					return armreservations.CalculateExchangeOperationResultResponse{}, nil
				})
				client.SetDoExchangeCaller(func(context.Context, string) (armreservations.ExchangeOperationResultResponse, error) {
					t.Fatal("a rejected source must not execute an exchange")
					return armreservations.ExchangeOperationResultResponse{}, nil
				})
				sources := make([]AzureExchangeSourceBody, len(tc.owned))
				for i := range tc.owned {
					sources[i] = AzureExchangeSourceBody{ReservationID: tc.owned[i].ReservationID, Quantity: 1}
				}
				h := azureScopeHandler(t, execute, client)
				req := azureScopeRequest(t, sources)
				var err error
				if execute {
					_, err = h.executeAzureExchange(context.Background(), req)
				} else {
					_, err = h.getAzureCompatibleOfferings(context.Background(), req)
				}
				require.Error(t, err)
				require.Equal(t, 1, client.listCalls)
				ce, ok := IsClientError(err)
				require.True(t, ok, "a refused source must be a client error, not a generic 500: %v", err)
				if tc.foreign {
					require.Equal(t, 403, ce.code)
					require.Nil(t, client.priced)
				} else {
					// The real CalculateExchange refuses with
					// ErrUnsupportedAppliedScope (#521); the caller gets a 422
					// naming the scope problem instead of "failed to ...".
					require.Equal(t, 422, ce.code)
					require.Contains(t, ce.message, "unsupported applied scope")
					require.NotContains(t, ce.message, "failed to")
					require.Len(t, client.priced, len(sources))
				}
			})
		}
	}
}
