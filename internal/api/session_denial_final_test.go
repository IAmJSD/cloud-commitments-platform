package api

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// Issue #173: without a token a session denial is final. The context principal is an authorized
// admin distinct from the denied bearer session, so a fall-through reaches the mutation.

const (
	denialTestExecID = "17317317-3173-1731-7317-317317317317"
	denialTestUserID = "denied-uid"
)

var errDenialTestReached = errors.New("mutation reached")

func newDenialTestHandler(t *testing.T, store *MockConfigStore, mockPurchase *MockPurchaseManager) (*Handler, context.Context) {
	t.Helper()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	mockAuth.On("ValidateSession", mock.Anything, "sess-tok").Return(&Session{UserID: denialTestUserID, Email: "denied@example.com"}, nil)
	mockAuth.On("HasPermissionAPI", mock.Anything, denialTestUserID, mock.Anything, mock.Anything).Return(false, nil)
	mockAuth.On("GetAllowedAccountsAPI", mock.Anything, denialTestUserID).Return(nil, nil).Maybe()
	mockAuth.On("ValidateCSRFToken", mock.Anything, "sess-tok", "csrf-ok").Return(nil).Maybe()

	store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{}, nil).Maybe()
	store.On("CancelExecutionAtomic", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(false, "", errDenialTestReached).Maybe()
	store.On("TransitionRIExchangeStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil, errDenialTestReached).Maybe()
	mockPurchase.On("ApproveAndExecute", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil, "", errDenialTestReached).Maybe()

	ctx := contextWithPrincipal(context.Background(), &Principal{
		Kind:    PrincipalSession,
		Session: &Session{UserID: apiKeyAdminUserID, Email: "admin@example.com"},
		UserID:  apiKeyAdminUserID,
	})
	return &Handler{auth: mockAuth, config: store, purchase: mockPurchase}, ctx
}

func denialTestRequest() *events.LambdaFunctionURLRequest {
	return &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer sess-tok", "x-csrf-token": "csrf-ok"},
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "POST"},
		},
	}
}

func requireForbidden(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a ClientError, got: %v", err)
	assert.Equal(t, 403, ce.code, "got: %v", err)
}

func TestApprovePurchase_SessionDenialWithoutTokenIsFinal(t *testing.T) {
	store, mockPurchase := new(MockConfigStore), new(MockPurchaseManager)
	t.Cleanup(func() { store.AssertExpectations(t) })
	h, ctx := newDenialTestHandler(t, store, mockPurchase)
	store.On("GetExecutionByID", mock.Anything, denialTestExecID).Return(&config.PurchaseExecution{
		ExecutionID:     denialTestExecID,
		Status:          "pending",
		Recommendations: []config.RecommendationRecord{{ID: "r1", Provider: "aws", UpfrontCost: 100}},
	}, nil)

	_, err := h.approvePurchase(ctx, denialTestRequest(), denialTestExecID, "")

	// The denial answers like a missing execution (issue #597).
	requireConcealedDenial(t, err, "requires approve-any or approve-own")
	mockPurchase.AssertNotCalled(t, "ApproveAndExecute", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestCancelPurchase_SessionDenialWithoutTokenIsFinal(t *testing.T) {
	store := new(MockConfigStore)
	t.Cleanup(func() { store.AssertExpectations(t) })
	h, ctx := newDenialTestHandler(t, store, new(MockPurchaseManager))
	store.On("GetExecutionByID", mock.Anything, denialTestExecID).Return(&config.PurchaseExecution{
		ExecutionID: denialTestExecID,
		Status:      "pending",
	}, nil)

	_, err := h.cancelPurchase(ctx, denialTestRequest(), denialTestExecID, "")

	requireConcealedDenial(t, err, "requires cancel-any or cancel-own")
	store.AssertNotCalled(t, "CancelExecutionAtomic", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestRevokePurchase_SessionDenialWithoutTokenIsFinal(t *testing.T) {
	store := new(MockConfigStore)
	t.Cleanup(func() { store.AssertExpectations(t) })
	h, ctx := newDenialTestHandler(t, store, new(MockPurchaseManager))
	store.On("GetExecutionByID", mock.Anything, denialTestExecID).Return(&config.PurchaseExecution{
		ExecutionID: denialTestExecID,
		Status:      "completed",
	}, nil)

	_, err := h.revokeViaEmailToken(ctx, denialTestRequest(), denialTestExecID, "")

	requireConcealedDenial(t, err, "requires cancel-any or cancel-own")
}

func TestApproveRIExchange_SessionDenialWithoutTokenIsFinal(t *testing.T) {
	store := new(MockConfigStore)
	h, ctx := newDenialTestHandler(t, store, new(MockPurchaseManager))
	store.On("GetRIExchangeRecord", mock.Anything, denialTestExecID).Return(&config.RIExchangeRecord{
		ID:            denialTestExecID,
		Status:        "pending",
		ApprovalToken: config.HashApprovalToken("tok"),
		SourceRIIDs:   []string{"ri-1"},
		PaymentDue:    "10.00",
	}, nil).Maybe()

	_, err := h.approveRIExchange(ctx, denialTestRequest(), denialTestExecID, "")

	requireForbidden(t, err)
	assert.Contains(t, err.Error(), "requires approve-any or approve-own")
	store.AssertNotCalled(t, "TransitionRIExchangeStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestApproveRIExchange_SessionDenialWithTokenUsesToken(t *testing.T) {
	store := new(MockConfigStore)
	t.Cleanup(func() { store.AssertExpectations(t) })
	h, ctx := newDenialTestHandler(t, store, new(MockPurchaseManager))
	store.On("GetRIExchangeRecord", mock.Anything, denialTestExecID).Return(&config.RIExchangeRecord{
		ID:            denialTestExecID,
		Status:        "pending",
		ApprovalToken: config.HashApprovalToken("tok"),
		SourceRIIDs:   []string{"ri-1"},
		PaymentDue:    "10.00",
	}, nil)

	_, err := h.approveRIExchange(ctx, denialTestRequest(), denialTestExecID, "tok")

	require.ErrorIs(t, err, errDenialTestReached)
	store.AssertCalled(t, "TransitionRIExchangeStatus", mock.Anything, denialTestExecID, "pending", "processing", (*string)(nil))
}
