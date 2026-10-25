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

// Issue #677: the stateless admin API key has no user UUID, so every pending
// purchase execution it minted was stored with created_by_user_id NULL and
// created_by_system false. Under 4-eyes, requireDifferentApprover and the
// manager's checkDifferentApprover deny that row for every approver, so it
// could only expire. The fix refuses the creating request while 4-eyes is on.
// These tests drive the three API writers end to end with the admin key.

const fourEyesAdminKey = "test-key"

func adminKeyRequest(body string) *events.LambdaFunctionURLRequest {
	return &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"x-api-key": fourEyesAdminKey},
		Body:    body,
	}
}

// errPersistReached is returned by a registered WithTx expectation in the
// mode-off controls: getting it back proves the request passed the new guard
// and reached the write.
var errPersistReached = NewClientError(418, "test sentinel: persistence reached")

func requireFourEyesUnattributedRefusal(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a clientError, got %v", err)
	assert.Equal(t, 403, ce.code)
	assert.Contains(t, ce.Error(), "no attributable user")
}

func TestExecutePurchase_AdminAPIKey_FourEyesOn_RefusedBeforeAnyWrite(t *testing.T) {
	for _, body := range map[string]string{
		"approval email": `{"recommendations":[{"id":"rec-1","provider":"aws","service":"ec2","count":1,"term":1,"payment":"all-upfront","upfront_cost":500.0,"savings":100.0}]}`,
		"direct execute": directExecRecBody,
	} {
		ctx := context.Background()
		store := new(MockConfigStore)
		store.On("GetGlobalConfig", ctx).Return(fourEyesCfgOn(), nil)
		expectStoredRecs(store, config.RecommendationRecord{
			ID: "rec-1", Provider: "aws", Service: "ec2", Count: 1, Term: 1, Payment: "all-upfront", UpfrontCost: 500, Savings: 100,
		})
		handler := &Handler{config: store, apiKey: fourEyesAdminKey}

		_, err := handler.executePurchase(ctx, adminKeyRequest(body))

		requireFourEyesUnattributedRefusal(t, err)
		store.AssertNotCalled(t, "WithTx")
		store.AssertNotCalled(t, "SavePurchaseExecutionTx")
		store.AssertNotCalled(t, "SavePurchaseExecution")
	}
}

func TestExecutePurchase_AdminAPIKey_FourEyesOff_ReachesPersistence(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	store.On("GetGlobalConfig", ctx).Return(fourEyesCfgOff(), nil)
	expectStoredRecs(store, config.RecommendationRecord{
		ID: "rec-1", Provider: "aws", Service: "ec2", Count: 1, Term: 1, Payment: "all-upfront", UpfrontCost: 500, Savings: 100,
	})
	store.On("WithTx", ctx, mock.Anything).Return(errPersistReached)
	handler := &Handler{config: store, apiKey: fourEyesAdminKey}

	_, err := handler.executePurchase(ctx, adminKeyRequest(`{"recommendations":[{"id":"rec-1","provider":"aws","service":"ec2","count":1,"term":1,"payment":"all-upfront","upfront_cost":500.0,"savings":100.0}]}`))

	require.ErrorIs(t, err, errPersistReached)
}

func TestRetryPurchase_UnattributedSession_FourEyesOn_RefusedBeforeAnyWrite(t *testing.T) {
	creator := retryOtherID
	failed := &config.PurchaseExecution{
		ExecutionID:     retryExecID,
		Status:          "failed",
		Error:           "send failed: transient SES throttle",
		CreatedByUserID: &creator,
		Recommendations: []config.RecommendationRecord{{Provider: "aws", Service: "rds", Term: 1, UpfrontCost: 100}},
	}
	ctx := context.Background()
	store := new(MockConfigStore)
	store.On("GetExecutionByID", ctx, retryExecID).Return(failed, nil)
	store.On("GetGlobalConfig", ctx).Return(fourEyesCfgOn(), nil)
	handler := &Handler{config: store, auth: unattributedRetryAuth(t)}

	_, err := handler.retryPurchase(ctx, sessionRetryReq(), retryExecID)

	requireFourEyesUnattributedRefusal(t, err)
	store.AssertNotCalled(t, "WithTx")
	store.AssertNotCalled(t, "SavePurchaseExecutionTx")
	store.AssertNotCalled(t, "LinkRetryExecutionAtomic")
}

func TestRetryPurchase_UnattributedSession_FourEyesOff_ReachesPersistence(t *testing.T) {
	creator := retryOtherID
	failed := &config.PurchaseExecution{
		ExecutionID:     retryExecID,
		Status:          "failed",
		Error:           "send failed: transient SES throttle",
		CreatedByUserID: &creator,
		Recommendations: []config.RecommendationRecord{{Provider: "aws", Service: "rds", Term: 1, UpfrontCost: 100}},
	}
	ctx := context.Background()
	store := new(MockConfigStore)
	store.On("GetExecutionByID", ctx, retryExecID).Return(failed, nil)
	store.On("GetGlobalConfig", ctx).Return(fourEyesCfgOff(), nil)
	store.On("WithTx", ctx, mock.Anything).Return(errPersistReached)
	handler := &Handler{config: store, auth: unattributedRetryAuth(t)}

	_, err := handler.retryPurchase(ctx, sessionRetryReq(), retryExecID)

	require.ErrorIs(t, err, errPersistReached)
}

// unattributedRetryAuth resolves the retry request's bearer token to a session
// whose UserID is not a user UUID. The retry route needs a bearer session, so
// the admin API key cannot reach it today (it gets a 401); this covers any
// session type that would still stamp a NULL creator onto the successor.
func unattributedRetryAuth(t *testing.T) *MockAuthService {
	t.Helper()
	session := &Session{UserID: "service-principal"}
	auth := new(MockAuthService)
	auth.On("ValidateSession", mock.Anything, "sess-tok").Return(session, nil)
	auth.On("HasPermissionAPI", mock.Anything, session.UserID, "retry-any", "purchases").Return(true, nil).Maybe()
	auth.On("GetAllowedAccountsAPI", mock.Anything, session.UserID).Return([]string{}, nil).Maybe()
	auth.allowConstraintChecks()
	return auth
}

const fourEyesPlanID = "11111111-1111-1111-1111-111111111111"

func TestCreatePlannedPurchases_AdminAPIKey_FourEyesOn_RefusedBeforeAnyWrite(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	store.On("GetGlobalConfig", ctx).Return(fourEyesCfgOn(), nil)
	handler := &Handler{config: store, apiKey: fourEyesAdminKey}

	_, err := handler.createPlannedPurchases(ctx, adminKeyRequest(`{"count": 1, "start_date": "2030-01-01"}`), fourEyesPlanID)

	requireFourEyesUnattributedRefusal(t, err)
	store.AssertNotCalled(t, "WithTx")
	store.AssertNotCalled(t, "SavePurchaseExecutionTx")
}

func TestCreatePlannedPurchases_AdminAPIKey_FourEyesOff_ReachesPersistence(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	store.On("GetGlobalConfig", ctx).Return(fourEyesCfgOff(), nil)
	store.On("GetPurchasePlan", ctx, fourEyesPlanID).Return(&config.PurchasePlan{ID: fourEyesPlanID, Enabled: true}, nil)
	store.On("WithTx", ctx, mock.Anything).Return(errPersistReached)
	handler := &Handler{config: store, apiKey: fourEyesAdminKey}

	_, err := handler.createPlannedPurchases(ctx, adminKeyRequest(`{"count": 1, "start_date": "2030-01-01"}`), fourEyesPlanID)

	require.ErrorIs(t, err, errPersistReached)
}

// The guard itself, for the cases the endpoint tests above do not reach.
func TestRequireAttributablePurchaseCreator(t *testing.T) {
	ctx := context.Background()
	adminKey := &Session{UserID: apiKeyAdminUserID}

	t.Run("user session passes without reading config", func(t *testing.T) {
		store := new(MockConfigStore)
		h := &Handler{config: store}
		require.NoError(t, h.requireAttributablePurchaseCreator(ctx, &Session{UserID: retryCallerID, Email: "u@example.com"}))
		require.NoError(t, h.requireAttributablePurchaseCreator(ctx, &Session{UserID: retryCallerID, UserAPIKeyID: "key-1"}))
		store.AssertNotCalled(t, "GetGlobalConfig")
	})

	t.Run("config load failure fails closed", func(t *testing.T) {
		store := new(MockConfigStore)
		store.On("GetGlobalConfig", ctx).Return(nil, errors.New("db down"))
		h := &Handler{config: store}
		err := h.requireAttributablePurchaseCreator(ctx, adminKey)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "4-eyes policy check")
	})

	t.Run("nil config means 4-eyes off", func(t *testing.T) {
		store := new(MockConfigStore)
		store.On("GetGlobalConfig", ctx).Return(nil, nil)
		h := &Handler{config: store}
		require.NoError(t, h.requireAttributablePurchaseCreator(ctx, adminKey))
	})

	t.Run("nil session under 4-eyes is refused", func(t *testing.T) {
		store := new(MockConfigStore)
		store.On("GetGlobalConfig", ctx).Return(fourEyesCfgOn(), nil)
		h := &Handler{config: store}
		requireFourEyesUnattributedRefusal(t, h.requireAttributablePurchaseCreator(ctx, nil))
	})

	t.Run("non-UUID user id under 4-eyes is refused", func(t *testing.T) {
		store := new(MockConfigStore)
		store.On("GetGlobalConfig", ctx).Return(fourEyesCfgOn(), nil)
		h := &Handler{config: store}
		requireFourEyesUnattributedRefusal(t, h.requireAttributablePurchaseCreator(ctx, &Session{UserID: "not-a-uuid"}))
	})
}
