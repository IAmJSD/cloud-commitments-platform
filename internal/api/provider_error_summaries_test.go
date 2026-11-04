package api

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/exchange"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const (
	fakeRoleARN   = "arn:aws:sts::123456789012:assumed-role/cudly-role/session"
	fakeRequestID = "11111111-2222-3333-4444-555555555555"
	fakeSubPath   = "/subscriptions/" + fakeSubID + "/providers/Microsoft.Capacity/reservationOrders/x"
)

// awsFault builds a smithy error shaped like the SDK's, with an ARN, account
// and request ID in its text.
func awsFault(code string) error {
	return fmt.Errorf("operation error EC2: Op, https response error StatusCode: 400, RequestID: %s, api error %s: %w",
		fakeRequestID, code, &smithy.GenericAPIError{
			Code:    code,
			Message: "User: " + fakeRoleARN + " in account 123456789012 request " + fakeRequestID,
			Fault:   smithy.FaultClient,
		})
}

func assertNoProviderDetail(t *testing.T, where, s string) {
	t.Helper()
	for _, n := range []string{"arn:aws", "123456789012", fakeRequestID, fakeSubID, fakeTenant, "management.azure.com"} {
		assert.NotContains(t, s, n, where)
	}
}

func TestExchangeErrors_HideProviderDetail(t *testing.T) {
	for _, code := range []string{"InvalidOfferingId", "InvalidParameter", "ValidationError"} {
		ce, ok := IsClientError(mapAWSExchangeError("exchange quote failed", awsFault(code)))
		require.True(t, ok)
		assert.Equal(t, 400, ce.code)
		assertNoProviderDetail(t, code, ce.message)
		assert.Contains(t, ce.message, "the exchange request was rejected as invalid")
	}
	ce, ok := IsClientError(mapAzureExchangeError("exchange failed", azureHTTPError(t, 400, fakeSubPath)))
	require.True(t, ok)
	assert.Equal(t, 400, ce.code)
	assertNoProviderDetail(t, "azure", ce.message)
	assert.Contains(t, ce.message, "the exchange request was rejected as invalid")
}

func TestExchangeApprovalFailure_HidesProviderDetail(t *testing.T) {
	ctx := context.Background()
	id := "550e8400-e29b-41d4-a716-000000000001"
	store := new(MockConfigStore)
	t.Cleanup(func() { store.AssertExpectations(t) })
	store.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{RIExchangeMaxDailyUSD: 1000, RIExchangeMaxPerExchangeUSD: 500}, nil)
	store.On("ReserveApprovedRIExchange", ctx, mock.Anything, mock.Anything, mock.Anything).Return("50.000000", nil)
	var stored string
	store.On("FailRIExchange", mock.Anything, id, mock.Anything).
		Run(func(args mock.Arguments) { stored = args.String(2) }).Return(nil)
	h := &Handler{config: store, executeExchangeFn: func(context.Context, exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
		return "", nil, errors.New("operation error EC2: Accept, https response error StatusCode: 403, RequestID: " + fakeRequestID +
			", api error UnauthorizedOperation: User: " + fakeRoleARN + " is not authorized")
	}}

	resp, err := h.executeApprovedExchange(ctx, id, &config.RIExchangeRecord{ID: id, Region: "us-east-1", SourceRIIDs: []string{"ri-1"}, TargetOfferingID: "o", TargetCount: 1})
	require.NoError(t, err)
	reason := resp.(map[string]any)["reason"].(string)
	assertNoProviderDetail(t, "response", reason)
	assertNoProviderDetail(t, "stored", stored)
	assert.Contains(t, reason, "not allowed")
}

func TestExchangeHistory_SanitizesStoredErrors(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	t.Cleanup(func() { store.AssertExpectations(t) })
	auth := new(MockAuthService)
	t.Cleanup(func() { auth.AssertExpectations(t) })
	auth.On("ValidateSession", ctx, "admin-token").Return(&Session{UserID: "uid"}, nil)
	auth.grantAdmin()
	raw := "api error UnauthorizedOperation: User: " + fakeRoleARN + " RequestID: " + fakeRequestID
	const note = "exchange record has no region; cannot execute safely"
	store.On("GetRIExchangeHistory", ctx, mock.Anything, 500).Return([]config.RIExchangeRecord{
		{ID: "a", Status: "failed", Error: raw},
		{ID: "b", Status: "failed", Error: note},
		{ID: "c", Status: "completed"},
	}, nil)

	h := &Handler{auth: auth, config: store}
	result, err := h.getRIExchangeHistory(ctx, &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer admin-token"}})
	require.NoError(t, err)
	recs := result.(*RIExchangeHistoryResponse).Records
	assertNoProviderDetail(t, "history", recs[0].Error)
	assert.Contains(t, recs[0].Error, "not allowed")
	assert.Equal(t, note, recs[1].Error)
	assert.Empty(t, recs[2].Error)
}
