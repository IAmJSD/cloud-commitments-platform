package api

// The purchases MaxPurchaseAmount cap is USD, so a purchase whose amounts are
// not provably USD is never compared against it (issue #361). The mock auth
// service answers constraint checks with the real
// auth.PermissionsAllowForConstraintSets, so these exercise the production
// matcher. Nothing here reaches a purchase manager or provider.

import (
	"context"
	"errors"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func currencyRec(upfront float64, currency string) config.RecommendationRecord {
	rec := capRec(upfront)
	rec.CommitmentCurrency = currency
	return rec
}

func requireCurrencyRefusal(t *testing.T, err error, currency string) {
	t.Helper()
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a clientError, got %v", err)
	assert.Equal(t, 403, ce.code)
	assert.Contains(t, ce.Error(), "USD spend cap")
	assert.Contains(t, ce.Error(), "priced in "+currency+" currency")
}

// TestEnforcePurchaseConstraints_NonUSDUnderCapRefused fails on main: there a
// 900 EUR (or 900 of an unknown currency) purchase cleared a 1,000 USD cap
// because 900 <= 1000.
func TestEnforcePurchaseConstraints_NonUSDUnderCapRefused(t *testing.T) {
	ctx := context.Background()
	session := &Session{UserID: "dddddddd-dddd-dddd-dddd-dddddddddddd"}
	for _, action := range []string{auth.ActionExecute, auth.ActionExecuteAny, auth.ActionApproveAny, auth.ActionApproveOwn} {
		for _, tc := range []struct{ currency, want string }{
			{"EUR", "EUR"}, {"JPY", "JPY"}, {"", "unknown"}, {"  ", "unknown"},
		} {
			t.Run(action+"/"+tc.want, func(t *testing.T) {
				h := handlerWithCap(action, 1000)
				err := h.enforcePurchaseConstraints(ctx, session, action, []config.RecommendationRecord{currencyRec(900, tc.currency)})
				requireCurrencyRefusal(t, err, tc.want)
			})
		}
	}
}

func TestEnforcePurchaseConstraints_MixedBatchRefused(t *testing.T) {
	h := handlerWithCap(auth.ActionExecute, 1000)
	session := &Session{UserID: "dddddddd-dddd-dddd-dddd-dddddddddddd"}
	err := h.enforcePurchaseConstraints(context.Background(), session, auth.ActionExecute,
		[]config.RecommendationRecord{currencyRec(100, "USD"), currencyRec(100, "CNY")})
	requireCurrencyRefusal(t, err, "CNY")
}

func TestEnforcePurchaseConstraints_USDStillComparedAgainstCap(t *testing.T) {
	ctx := context.Background()
	session := &Session{UserID: "dddddddd-dddd-dddd-dddd-dddddddddddd"}
	h := handlerWithCap(auth.ActionExecute, 1000)
	for _, currency := range []string{"USD", "usd", " USD "} {
		require.NoError(t, h.enforcePurchaseConstraints(ctx, session, auth.ActionExecute, []config.RecommendationRecord{currencyRec(900, currency)}))
	}
	err := h.enforcePurchaseConstraints(ctx, session, auth.ActionExecute, []config.RecommendationRecord{currencyRec(1100, "USD")})
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, 403, ce.code)
	assert.Contains(t, ce.Error(), "exceeds the constraints")
}

// A grant with no amount cap has nothing to compare, so a non-USD purchase
// is not penalized.
func TestEnforcePurchaseConstraints_NonUSDWithoutCapAllowed(t *testing.T) {
	h := handlerWithCap(auth.ActionExecute, 0)
	session := &Session{UserID: "dddddddd-dddd-dddd-dddd-dddddddddddd"}
	for _, currency := range []string{"EUR", ""} {
		require.NoError(t, h.enforcePurchaseConstraints(context.Background(), session, auth.ActionExecute,
			[]config.RecommendationRecord{currencyRec(1_000_000, currency)}))
	}
}

// When another dimension refuses the request too, that dimension's error is
// returned, not the currency one.
func TestEnforcePurchaseConstraints_NonUSDOtherDimensionKeepsItsError(t *testing.T) {
	mockAuth := new(MockAuthService)
	mockAuth.grantPermissions([]auth.Permission{{
		Action: auth.ActionExecute, Resource: auth.ResourcePurchases,
		Constraints: &auth.PermissionConstraints{MaxPurchaseAmount: 1000, Regions: []string{"eu-west-1"}},
	}})
	h := &Handler{auth: mockAuth}
	session := &Session{UserID: "dddddddd-dddd-dddd-dddd-dddddddddddd"}
	err := h.enforcePurchaseConstraints(context.Background(), session, auth.ActionExecute,
		[]config.RecommendationRecord{currencyRec(900, "EUR")})
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, 403, ce.code)
	assert.Contains(t, ce.Error(), "exceeds the constraints")
	assert.NotContains(t, ce.Error(), "USD spend cap")
}

// TestHandler_executePurchase_NonUSDStoredPriceUnderCapRefused drives the web
// execute path: the currency comes from the stored row, never from the
// request, so a client stating "USD" cannot lift the refusal.
func TestHandler_executePurchase_NonUSDStoredPriceUnderCapRefused(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	authService := new(MockAuthService)
	authService.On("ValidateSession", ctx, "eur-cap").Return(&Session{UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Email: "capped@example.com"}, nil)
	authService.grantPermissions([]auth.Permission{{Action: auth.ActionExecute, Resource: auth.ResourcePurchases, Constraints: &auth.PermissionConstraints{MaxPurchaseAmount: 1000}}})
	store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{}, nil).Maybe()
	store.On("GetPendingExecutions", mock.Anything).Return([]config.PurchaseExecution{}, nil).Maybe()
	expectStoredRecs(store, config.RecommendationRecord{
		ID: "eur-rec", Provider: "azure", Service: "compute", Region: "westeurope", ResourceType: "Standard_D2s_v3",
		Count: 1, Term: 3, Payment: "upfront", UpfrontCost: 900, Savings: 50, CommitmentCurrency: "EUR",
	})
	h := &Handler{config: store, auth: authService}
	_, err := h.executePurchase(ctx, &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"Authorization": "Bearer eur-cap"},
		Body:    `{"recommendations":[{"id":"eur-rec","provider":"azure","service":"compute","region":"westeurope","resource_type":"Standard_D2s_v3","count":1,"term":3,"payment":"all-upfront","upfront_cost":900,"savings":50,"commitment_currency":"USD"}]}`,
	})
	requireCurrencyRefusal(t, err, "EUR")
	store.AssertNotCalled(t, "SavePurchaseExecution", mock.Anything, mock.Anything)
}

// An auth backend failure is returned as is: it is not a denial, so it is
// neither retried nor reported as a currency refusal.
func TestEnforcePurchaseConstraints_NonUSDBackendErrorPropagates(t *testing.T) {
	boom := errors.New("auth store unavailable")
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("HasPermissionForConstraintsAPI", mock.Anything, mock.Anything, auth.ActionExecute, auth.ResourcePurchases, mock.Anything).
		Return(false, boom).Once()
	h := &Handler{auth: mockAuth}
	session := &Session{UserID: "dddddddd-dddd-dddd-dddd-dddddddddddd"}
	err := h.enforcePurchaseConstraints(context.Background(), session, auth.ActionExecute,
		[]config.RecommendationRecord{currencyRec(900, "EUR")})
	require.ErrorIs(t, err, boom)
	_, isClient := IsClientError(err)
	assert.False(t, isClient)
	mockAuth.AssertNumberOfCalls(t, "HasPermissionForConstraintsAPI", 1)
}
