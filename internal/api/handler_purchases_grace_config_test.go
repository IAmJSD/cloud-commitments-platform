package api

import (
	"context"
	"errors"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Fixture/mock evidence: in-memory mocks, no database.
//
// Audit A01-016 (issue #111): the grace periods that size purchase
// suppressions come from the global config. A failed read used to fall back
// to the 7-day default, writing suppressions for a provider the operator had
// switched off (grace 0) or with the wrong expiry. Both call sites now fail
// before persisting anything, as internal/purchase does.

func TestHandler_executePurchase_GlobalConfigErrorFailsBeforePersist(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	session := &Session{UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Email: "admin@example.com"}
	mockAuth.On("ValidateSession", ctx, "admin-token").Return(session, nil)
	mockAuth.grantAdminPurchaser()
	mockStore.On("GetGlobalConfig", ctx).Return(nil, errors.New("config db down"))
	mockStore.On("SavePurchaseExecution", ctx, mock.AnythingOfType("*config.PurchaseExecution")).Return(nil).Maybe()
	mockStore.On("GetPendingExecutions", ctx).Return([]config.PurchaseExecution{}, nil).Maybe()
	expectStoredRecs(mockStore, config.RecommendationRecord{
		Provider: "aws", Service: "ec2", ResourceType: "m5.large", Count: 1, Term: 1, Payment: "all-upfront", UpfrontCost: 100, Savings: 50,
	})
	handler := &Handler{config: mockStore, auth: mockAuth}

	result, err := handler.executePurchase(ctx, &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"Authorization": "Bearer admin-token"},
		Body:    `{"recommendations": [{"id": "", "provider": "aws", "service": "ec2", "resource_type": "m5.large", "count": 1, "term": 1, "payment": "all-upfront", "upfront_cost": 100.0, "savings": 50.0}]}`,
	})

	require.ErrorContains(t, err, "config db down")
	assert.Nil(t, result)
	_, isClient := IsClientError(err)
	assert.False(t, isClient, "a config read failure is a server error")
	mockStore.AssertNotCalled(t, "SavePurchaseExecution", mock.Anything, mock.Anything)
	mockStore.AssertNotCalled(t, "CreateSuppressionTx", mock.Anything, mock.Anything, mock.Anything)
}

func TestHandler_retryPurchase_GlobalConfigErrorFailsBeforePersist(t *testing.T) {
	_, handler, mockConfig := retryDedupeFixture(t, false)
	for _, c := range mockConfig.ExpectedCalls {
		if c.Method == "GetGlobalConfig" {
			c.Unset()
		}
	}
	mockConfig.On("GetGlobalConfig", mock.Anything).Return(nil, errors.New("config db down"))
	mockConfig.On("ListRecentSubmitsTx", mock.Anything, mock.Anything, retryCallerID, mock.AnythingOfType("time.Time")).
		Return([]config.PurchaseExecution{}, nil).Maybe()

	result, err := handler.retryPurchase(context.Background(), sessionRetryReq(), retryExecID)

	require.ErrorContains(t, err, "config db down")
	assert.Nil(t, result)
	_, isClient := IsClientError(err)
	assert.False(t, isClient, "a config read failure is a server error")
	mockConfig.AssertNotCalled(t, "SavePurchaseExecution", mock.Anything, mock.Anything)
	mockConfig.AssertNotCalled(t, "LinkRetryExecutionAtomic", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	mockConfig.AssertNotCalled(t, "CreateSuppressionTx", mock.Anything, mock.Anything, mock.Anything)
}
