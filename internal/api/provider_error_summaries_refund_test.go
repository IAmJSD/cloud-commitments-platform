package api

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestMarketplaceErrors_HideProviderDetail(t *testing.T) {
	ce, ok := IsClientError(mapAWSMarketplaceError("create listing", awsFault("InvalidParameterValue")))
	require.True(t, ok)
	assert.Equal(t, 400, ce.code)
	assertNoProviderDetail(t, "400", ce.message)

	server := fmt.Errorf("operation error EC2: Op, https response error StatusCode: 503, RequestID: %s, api error Unavailable: %w", fakeRequestID,
		&smithy.GenericAPIError{Code: "Unavailable", Message: "account 123456789012 " + fakeRoleARN, Fault: smithy.FaultServer})
	ce, ok = IsClientError(mapAWSMarketplaceError("AWS marketplace listing failed", server))
	require.True(t, ok)
	assert.Equal(t, 502, ce.code)
	assertNoProviderDetail(t, "502", ce.message)
	assert.Contains(t, ce.message, "AWS marketplace listing failed")
}

func TestMarketplaceRollbackFailure_HidesProviderDetail(t *testing.T) {
	ctx := context.Background()
	const purchaseID = "22222222-2222-2222-2222-222222222222"
	store := new(MockConfigStore)
	store.On("UpdatePurchaseHistoryListing", ctx, purchaseID, "lst-1", mock.Anything).Return(nil)
	h := &Handler{config: store}
	err := h.keepUncanceledListing(ctx, purchaseID, ec2MarketplaceListingResult{ListingID: "lst-1", State: config.ListingStateActive}, awsFault("UnauthorizedOperation"))
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assertNoProviderDetail(t, "rollback", ce.message)
	assert.Contains(t, ce.message, "lst-1")
	assert.Contains(t, ce.message, "reference: purchase "+purchaseID)
}

func TestAzureRefundErrors_HideProviderDetail(t *testing.T) {
	ctx := context.Background()
	raw := azureHTTPError(t, 400, fakeSubPath)

	t.Run("calculate step of revoke", func(t *testing.T) {
		h := &Handler{}
		_, _, _, err := h.azureCalculateRefund(ctx, &stubCalcRefundClient{err: raw}, "p-1", "order", "res", 1)
		ce, ok := IsClientError(err)
		require.True(t, ok)
		assert.Equal(t, 400, ce.code)
		assertNoProviderDetail(t, "calculate", ce.message)
		assert.Contains(t, ce.message, "reference: purchase p-1")
		assert.Contains(t, ce.message, "Azure refund calculation rejected")
	})

	t.Run("return step", func(t *testing.T) {
		store := new(MockConfigStore)
		store.On("ClearRevocationInFlight", mock.Anything, mock.Anything).Return(nil)
		h := &Handler{config: store}
		r := armReservationRecord()
		_, err := h.callAzureReturn(ctx, &stubCalcRefundClientWithAmount{amount: 30, currency: "USD", sessID: "s"},
			&stubReturnClient{err: raw},
			r, "order-abc", "res-xyz", confirmedQuote(30, "USD"))
		ce, ok := IsClientError(err)
		require.True(t, ok)
		assertNoProviderDetail(t, "return", ce.message)
		assert.Contains(t, ce.message, "reference: purchase "+r.PurchaseID)
	})

	t.Run("quote endpoint", func(t *testing.T) {
		store := new(MockConfigStore)
		auth := new(MockAuthService)
		auth.On("ValidateSession", ctx, "tok").Return(revokeAdminSession(), nil)
		r := armReservationRecord()
		store.On("GetPurchaseHistoryByPurchaseID", ctx, r.PurchaseID).Return(r, nil)
		h := &Handler{config: store, auth: auth, azureRevokeFactory: &azureRevokeClientFactory{
			newCredential: func() (azcore.TokenCredential, error) { return nil, nil },
			newCalculateRefundClient: func(azcore.TokenCredential) (azureCalculateRefundClient, error) {
				return &stubCalcRefundClient{err: raw}, nil
			},
			newReturnClient: func(azcore.TokenCredential) (azureReturnClient, error) { return &stubReturnClient{}, nil },
		}}
		_, err := h.calculateAzureRevoke(ctx, sessionReq("tok"), r.PurchaseID)
		ce, ok := IsClientError(err)
		require.True(t, ok)
		assert.Equal(t, 400, ce.code)
		assertNoProviderDetail(t, "quote", ce.message)
	})
}

func TestSessionApproveScheduleFailure_HidesStoreError(t *testing.T) {
	ctx := context.Background()
	const execID = "00000000-0000-0000-0000-000000000000"
	store := new(MockConfigStore)
	store.On("ScheduleApprovedExecution", ctx, execID, mock.Anything, mock.Anything, "", mock.Anything, mock.Anything).
		Return(nil, errors.New("pq: password authentication failed for user cudly at db.internal.example:5432"))
	h := &Handler{config: store}
	_, err := h.approveWithDelay(ctx, &config.PurchaseExecution{ExecutionID: execID, Status: "pending"}, 48*time.Hour, "a@example.com", nil, "")
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, 500, ce.code)
	assert.NotContains(t, ce.message, "db.internal.example")
	assert.NotContains(t, ce.message, "pq:")
	assert.Contains(t, ce.message, "reference: execution "+execID)
}
