package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/exchange"
	"github.com/LeanerCloud/cloud-commitments-go/providers/aws/recommendations"
	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
	azurecompute "github.com/LeanerCloud/cloud-commitments-go/providers/azure/services/compute"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/ricost"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestListConvertibleRIs_RequiresAdmin(t *testing.T) {
	h := &Handler{} // no auth configured
	_, err := h.listConvertibleRIs(context.Background(), &events.LambdaFunctionURLRequest{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "authentication")
}

// issue #871: the AWS convertible-RI list must honor the Main Header global
// account filter. When the ?account_id= chip selects an account other than the
// running (ambient) AWS account, none of these RIs belong to it, so the
// handler returns an empty list without touching AWS config.
func TestListConvertibleRIs_AccountScopeMismatchReturnsEmpty(t *testing.T) {
	mockAuth := &mockAuthForExchange{}
	h := &Handler{
		auth: mockAuth,
		// Running account differs from the requested account_id.
		riInstancesAccountResolver: func(_ context.Context) (string, error) {
			return "999999999999", nil
		},
	}

	resp, err := h.listConvertibleRIs(context.Background(), &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"authorization": "Bearer test-token"},
		QueryStringParameters: map[string]string{"account_id": "123456789012"},
	})
	require.NoError(t, err)
	typed, ok := resp.(*ConvertibleRIsResponse)
	require.True(t, ok, "expected *ConvertibleRIsResponse, got %T", resp)
	assert.Empty(t, typed.Instances, "RIs from a different account must not leak under an account_id filter")
}

// issue #871: a real STS resolution failure must fail closed (propagate the
// error) rather than silently falling through to the unscoped fleet.
func TestListConvertibleRIs_AccountResolveErrorFailsClosed(t *testing.T) {
	mockAuth := &mockAuthForExchange{}
	h := &Handler{
		auth: mockAuth,
		riInstancesAccountResolver: func(_ context.Context) (string, error) {
			return "", fmt.Errorf("sts get-caller-identity denied")
		},
	}

	_, err := h.listConvertibleRIs(context.Background(), &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"authorization": "Bearer test-token"},
		QueryStringParameters: map[string]string{"account_id": "123456789012"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolve running AWS account")
}

func TestGetRIUtilization_RequiresAdmin(t *testing.T) {
	h := &Handler{}
	_, err := h.getRIUtilization(context.Background(), &events.LambdaFunctionURLRequest{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "authentication")
}

func TestGetReshapeRecommendations_RequiresAdmin(t *testing.T) {
	h := &Handler{}
	_, err := h.getReshapeRecommendations(context.Background(), &events.LambdaFunctionURLRequest{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "authentication")
}

func TestGetExchangeQuote_RequiresAdmin(t *testing.T) {
	h := &Handler{}
	_, err := h.getExchangeQuote(context.Background(), &events.LambdaFunctionURLRequest{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "authentication")
}

func TestExecuteExchange_RequiresAdmin(t *testing.T) {
	h := &Handler{}
	_, err := h.executeExchange(context.Background(), &events.LambdaFunctionURLRequest{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "authentication")
}

func TestGetExchangeQuote_Validation(t *testing.T) {
	// Test with a mock auth that always succeeds
	mockAuth := &mockAuthForExchange{}
	h := &Handler{auth: mockAuth}

	// Missing body
	_, err := h.getExchangeQuote(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer test-token"},
		Body:    "{}",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ri_ids is required")

	// Missing both target_offering_id and targets[]
	_, err = h.getExchangeQuote(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer test-token"},
		Body:    `{"ri_ids":["ri-123"]}`,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "targets[] or target_offering_id is required")

	// Empty offering_id inside targets[] is rejected per-entry so a
	// caller can't sneak through with a zero-valued target.
	_, err = h.getExchangeQuote(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer test-token"},
		Body:    `{"ri_ids":["ri-123"],"targets":[{"offering_id":"","count":1}]}`,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "targets[0].offering_id is required")
}

func TestGetRIUtilization_LookbackValidation(t *testing.T) {
	mockAuth := &mockAuthForExchange{}
	h := &Handler{auth: mockAuth}

	tests := []struct {
		name    string
		days    string
		wantErr string
	}{
		{"negative", "-1", "lookback_days must be between 1 and 365"},
		{"zero", "0", "lookback_days must be between 1 and 365"},
		{"too large", "999", "lookback_days must be between 1 and 365"},
		{"not a number", "abc", "lookback_days must be between 1 and 365"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.getRIUtilization(context.Background(), &events.LambdaFunctionURLRequest{
				Headers:               map[string]string{"authorization": "Bearer test-token"},
				QueryStringParameters: map[string]string{"lookback_days": tt.days},
			})
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestGetReshapeRecommendations_LookbackValidation(t *testing.T) {
	mockAuth := &mockAuthForExchange{}
	h := &Handler{auth: mockAuth}

	tests := []struct {
		name    string
		days    string
		wantErr string
	}{
		{"negative", "-1", "lookback_days must be between 1 and 365"},
		{"zero", "0", "lookback_days must be between 1 and 365"},
		{"too large", "999", "lookback_days must be between 1 and 365"},
		{"not a number", "abc", "lookback_days must be between 1 and 365"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.getReshapeRecommendations(context.Background(), &events.LambdaFunctionURLRequest{
				Headers:               map[string]string{"authorization": "Bearer test-token"},
				QueryStringParameters: map[string]string{"lookback_days": tt.days},
			})
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestGetReshapeRecommendations_ThresholdValidation(t *testing.T) {
	mockAuth := &mockAuthForExchange{}
	h := &Handler{auth: mockAuth}

	tests := []struct {
		name    string
		value   string
		wantErr string
	}{
		{"negative", "-1", "threshold must be a number between 0 and 100"},
		{"over 100", "101", "threshold must be a number between 0 and 100"},
		{"not a number", "abc", "threshold must be a number between 0 and 100"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.getReshapeRecommendations(context.Background(), &events.LambdaFunctionURLRequest{
				Headers:               map[string]string{"authorization": "Bearer test-token"},
				QueryStringParameters: map[string]string{"threshold": tt.value},
			})
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestExecuteExchange_Validation(t *testing.T) {
	mockAuth := &mockAuthForExchange{}
	h := &Handler{auth: mockAuth}

	// Missing max_payment_due_usd
	_, err := h.executeExchange(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer test-token"},
		Body:    `{"ri_ids":["ri-123"],"target_offering_id":"offering-1","target_count":1}`,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "max_payment_due_usd is required")

	// Invalid max_payment_due_usd -- region provided so validation reaches the ParseDecimalRat check.
	_, err = h.executeExchange(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer test-token"},
		Body:    `{"ri_ids":["ri-123"],"target_offering_id":"offering-1","target_count":1,"max_payment_due_usd":"abc","region":"us-east-1"}`,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid max_payment_due_usd")

	// Missing both target_offering_id and targets[]
	_, err = h.executeExchange(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer test-token"},
		Body:    `{"ri_ids":["ri-123"],"max_payment_due_usd":"10"}`,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "targets[] or target_offering_id is required")

	// Empty offering_id inside targets[] rejected.
	_, err = h.executeExchange(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer test-token"},
		Body:    `{"ri_ids":["ri-123"],"targets":[{"offering_id":"","count":1}],"max_payment_due_usd":"10"}`,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "targets[0].offering_id is required")
}

// --- Approve/Reject edge case tests ---

func TestRejectRIExchange_AlreadyCompleted(t *testing.T) {
	mockStore := new(MockConfigStore)
	h := &Handler{config: mockStore}
	ctx := context.Background()
	id := "550e8400-e29b-41d4-a716-446655440000"
	token := "valid-token-123"

	// Record exists but is already completed
	mockStore.On("GetRIExchangeRecord", ctx, id).Return(&config.RIExchangeRecord{
		ID:            id,
		ApprovalToken: config.HashApprovalToken(token),
		Status:        "completed",
		ExchangeID:    "exch-already-done",
	}, nil)

	// Transition from pending->canceled fails (record is not pending).
	// Uses canonical spelling; see config.StatusCanceled.
	mockStore.On("TransitionRIExchangeStatus", ctx, id, "pending", config.StatusCanceled, mock.Anything).
		Return((*config.RIExchangeRecord)(nil), nil)

	_, err := h.rejectRIExchange(ctx, id, token)
	assert.Error(t, err)
	ce, ok := IsClientError(err)
	assert.True(t, ok)
	assert.Equal(t, 409, ce.code)
	assert.Contains(t, err.Error(), "already processed")

	mockStore.AssertExpectations(t)
}

func TestApproveRIExchange_AlreadyCancelled(t *testing.T) {
	mockStore := new(MockConfigStore)
	h := &Handler{config: mockStore}
	ctx := context.Background()
	id := "550e8400-e29b-41d4-a716-446655440001"
	token := "valid-token-456"

	// Record exists but was canceled by a newer analysis run
	mockStore.On("GetRIExchangeRecord", ctx, id).Return(&config.RIExchangeRecord{
		ID:            id,
		ApprovalToken: config.HashApprovalToken(token),
		Status:        "cancelled",
	}, nil)

	// Transition from pending→processing fails (record is canceled)
	mockStore.On("TransitionRIExchangeStatus", ctx, id, "pending", "processing", mock.Anything).
		Return((*config.RIExchangeRecord)(nil), nil)

	_, err := h.approveRIExchange(ctx, &events.LambdaFunctionURLRequest{}, id, token)
	assert.Error(t, err)
	ce, ok := IsClientError(err)
	assert.True(t, ok)
	assert.Equal(t, 409, ce.code)
	assert.Contains(t, err.Error(), "already processed")

	mockStore.AssertExpectations(t)
}

func TestApproveRIExchange_DoubleApprove(t *testing.T) {
	mockStore := new(MockConfigStore)
	h := &Handler{config: mockStore}
	ctx := context.Background()
	id := "550e8400-e29b-41d4-a716-446655440002"
	token := "valid-token-789"

	// Record exists and is already being processed (first approve succeeded)
	mockStore.On("GetRIExchangeRecord", ctx, id).Return(&config.RIExchangeRecord{
		ID:            id,
		ApprovalToken: config.HashApprovalToken(token),
		Status:        "processing",
		SourceRIIDs:   []string{"ri-123"},
		PaymentDue:    "5.00",
	}, nil)

	// Transition from pending→processing fails (already processing)
	mockStore.On("TransitionRIExchangeStatus", ctx, id, "pending", "processing", mock.Anything).
		Return((*config.RIExchangeRecord)(nil), nil)

	_, err := h.approveRIExchange(ctx, &events.LambdaFunctionURLRequest{}, id, token)
	assert.Error(t, err)
	ce, ok := IsClientError(err)
	assert.True(t, ok)
	assert.Equal(t, 409, ce.code)
	assert.Contains(t, err.Error(), "already processed")

	mockStore.AssertExpectations(t)
}

func TestApproveRIExchange_InvalidToken(t *testing.T) {
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	h := &Handler{config: mockStore}
	ctx := context.Background()
	id := "550e8400-e29b-41d4-a716-446655440003"

	mockStore.On("GetRIExchangeRecord", ctx, id).Return(&config.RIExchangeRecord{
		ID:            id,
		ApprovalToken: "correct-token",
		Status:        "pending",
	}, nil)

	_, err := h.approveRIExchange(ctx, &events.LambdaFunctionURLRequest{}, id, "wrong-token")
	assert.Error(t, err)
	ce, ok := IsClientError(err)
	assert.True(t, ok)
	assert.Equal(t, 403, ce.code)
	assert.Contains(t, err.Error(), "invalid approval token")
}

// TestApproveRIExchange_SessionAdmin verifies that an admin session can approve
// a pending RI exchange without an email token (issue #300).
func TestApproveRIExchange_SessionAdmin(t *testing.T) {
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	h := &Handler{config: mockStore, auth: mockAuth}
	ctx := context.Background()
	id := "550e8400-e29b-41d4-a716-446655440010"
	creatorID := "creator-uuid"

	adminSession := &Session{UserID: "admin-uuid", Email: "admin@example.com"}
	mockAuth.On("ValidateSession", ctx, "admin-bearer").Return(adminSession, nil)
	mockAuth.grantAdminPurchaser()
	// approveRIExchangeViaSession validates CSRF before anything else (issue
	// #1757); a valid token must be supplied for this session-authed path
	// to reach the record fetch at all.
	mockAuth.On("ValidateCSRFToken", ctx, "admin-bearer", "csrf-abc").Return(nil)

	// authorizeSessionApproveRIExchange: admin role short-circuits (no HasPermissionAPI call)

	// approveRIExchangeViaSession fetches the record to check status
	mockStore.On("GetRIExchangeRecord", ctx, id).Return(&config.RIExchangeRecord{
		ID:              id,
		Status:          "pending",
		ApprovalToken:   "tok",
		SourceRIIDs:     []string{"ri-123"},
		PaymentDue:      "100.00",
		CreatedByUserID: &creatorID,
	}, nil).Once()

	mockStore.On("TransitionRIExchangeStatus", ctx, id, "pending", "processing", mock.Anything).
		Return(&config.RIExchangeRecord{ID: id, Status: "processing", SourceRIIDs: []string{"ri-123"}, PaymentDue: "100.00"}, nil)
	// The record carries no region, so execution fails closed at the region
	// guard before ReserveApprovedRIExchange: the approval ends in FailRIExchange.
	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{
		RIExchangeMaxDailyUSD:       1000,
		RIExchangeMaxPerExchangeUSD: 500,
	}, nil)
	// executeApprovedExchange will call failExchange (no real AWS SDK) which
	// returns a non-error result map. Since execErr == nil, StampRIExchangeApprovedBy
	// is called as best-effort audit trail.
	mockStore.On("FailRIExchange", mock.Anything, id, mock.AnythingOfType("string")).Return(nil)
	mockStore.On("StampRIExchangeApprovedBy", mock.Anything, id, adminSession.Email).Return(nil)

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{
			"authorization": "Bearer admin-bearer",
			"x-csrf-token":  "csrf-abc",
		},
	}
	_, err := h.approveRIExchange(ctx, req, id, "")
	require.NoError(t, err)
	// The exchange execution will fail (no real AWS SDK) but we verify the
	// dispatch reached the session-authed path.
	mockStore.AssertCalled(t, "GetRIExchangeRecord", ctx, id)
	mockStore.AssertCalled(t, "TransitionRIExchangeStatus", ctx, id, "pending", "processing", mock.Anything)
	mockStore.AssertCalled(t, "FailRIExchange", mock.Anything, id, mock.AnythingOfType("string"))
	mockStore.AssertCalled(t, "StampRIExchangeApprovedBy", mock.Anything, id, adminSession.Email)
}

// TestApproveRIExchange_SessionApproveOwn verifies that a user with approve-own
// can approve a pending exchange they created, but not one created by another user.
func TestApproveRIExchange_SessionApproveOwn(t *testing.T) {
	ctx := context.Background()
	id := "550e8400-e29b-41d4-a716-446655440011"
	ownerID := "owner-uuid"
	otherID := "other-uuid"

	t.Run("own exchange allowed", func(t *testing.T) {
		mockStore := new(MockConfigStore)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		mockAuth := new(MockAuthService)
		t.Cleanup(func() { mockAuth.AssertExpectations(t) })
		h := &Handler{config: mockStore, auth: mockAuth}

		ownerSession := &Session{UserID: ownerID, Email: "owner@example.com"}
		mockAuth.On("ValidateSession", ctx, "owner-bearer").Return(ownerSession, nil)
		mockAuth.On("HasPermissionAPI", ctx, ownerID, auth.ActionApproveAny, auth.ResourcePurchases).Return(false, nil)
		mockAuth.On("HasPermissionAPI", ctx, ownerID, auth.ActionApproveOwn, auth.ResourcePurchases).Return(true, nil)
		mockAuth.On("GetAllowedAccountsAPI", ctx, ownerID).Return(nil, nil)
		// Issue #1757: CSRF must be validated before the record fetch.
		mockAuth.On("ValidateCSRFToken", ctx, "owner-bearer", "csrf-abc").Return(nil)

		// authorizeSessionApproveRIExchange fetches record for ownership check
		mockStore.On("GetRIExchangeRecord", ctx, id).Return(&config.RIExchangeRecord{
			ID:              id,
			Status:          "pending",
			SourceRIIDs:     []string{"ri-1"},
			PaymentDue:      "50.00",
			ApprovalToken:   "tok",
			CreatedByUserID: &ownerID,
		}, nil)

		mockStore.On("TransitionRIExchangeStatus", ctx, id, "pending", "processing", mock.Anything).
			Return(&config.RIExchangeRecord{ID: id, Status: "processing", SourceRIIDs: []string{"ri-1"}, PaymentDue: "50.00"}, nil)
		// The record carries no region, so execution fails closed at the region
		// guard before ReserveApprovedRIExchange: the approval ends in FailRIExchange.
		mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{
			RIExchangeMaxDailyUSD:       1000,
			RIExchangeMaxPerExchangeUSD: 500,
		}, nil)
		mockStore.On("FailRIExchange", mock.Anything, id, mock.AnythingOfType("string")).Return(nil)
		mockStore.On("StampRIExchangeApprovedBy", mock.Anything, id, ownerSession.Email).Return(nil)

		req := &events.LambdaFunctionURLRequest{
			Headers: map[string]string{
				"authorization": "Bearer owner-bearer",
				"x-csrf-token":  "csrf-abc",
			},
		}
		_, err := h.approveRIExchange(ctx, req, id, "")
		require.NoError(t, err)
		mockStore.AssertCalled(t, "TransitionRIExchangeStatus", ctx, id, "pending", "processing", mock.Anything)
	})

	t.Run("other user exchange rejected", func(t *testing.T) {
		mockStore := new(MockConfigStore)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		mockAuth := new(MockAuthService)
		t.Cleanup(func() { mockAuth.AssertExpectations(t) })
		h := &Handler{config: mockStore, auth: mockAuth}

		ownerSession := &Session{UserID: ownerID, Email: "owner@example.com"}
		mockAuth.On("ValidateSession", ctx, "owner-bearer").Return(ownerSession, nil)
		mockAuth.On("HasPermissionAPI", ctx, ownerID, auth.ActionApproveAny, auth.ResourcePurchases).Return(false, nil)
		mockAuth.On("HasPermissionAPI", ctx, ownerID, auth.ActionApproveOwn, auth.ResourcePurchases).Return(true, nil)
		mockAuth.On("GetAllowedAccountsAPI", ctx, ownerID).Return(nil, nil)
		// Issue #1757: CSRF must be validated before the record fetch, so this
		// test needs a valid token to reach the ownership check it targets.
		mockAuth.On("ValidateCSRFToken", ctx, "owner-bearer", "csrf-abc").Return(nil)

		// authorizeSessionApproveRIExchange fetches record — creator does not match
		mockStore.On("GetRIExchangeRecord", ctx, id).Return(&config.RIExchangeRecord{
			ID:              id,
			Status:          "pending",
			ApprovalToken:   "tok",
			CreatedByUserID: &otherID,
		}, nil)

		req := &events.LambdaFunctionURLRequest{
			Headers: map[string]string{
				"authorization": "Bearer owner-bearer",
				"x-csrf-token":  "csrf-abc",
			},
		}
		_, err := h.approveRIExchange(ctx, req, id, "")
		require.Error(t, err)
		ce, ok := IsClientError(err)
		require.True(t, ok)
		assert.Equal(t, 403, ce.code)
		assert.Contains(t, err.Error(), "cannot approve another user's pending exchange")
	})
}

// TestApproveRIExchange_SessionScope (issue #519): a user scoped to account A
// holding approve-any must not approve an exchange on a deployment whose cloud
// account is B, matching executeExchange's scope gate (#93).
func TestApproveRIExchange_SessionScope(t *testing.T) {
	ctx := context.Background()
	id := "550e8400-e29b-41d4-a716-446655440019"
	const accA, accB = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"

	setup := func(t *testing.T, deploymentAccount string, action string) (*Handler, *MockConfigStore) {
		mockStore := new(MockConfigStore)
		mockAuth := new(MockAuthService)
		t.Cleanup(func() { mockAuth.AssertExpectations(t) })
		mockAuth.On("ValidateSession", ctx, "sess").Return(&Session{UserID: "approver-uuid", Email: "approver@example.com"}, nil)
		mockAuth.On("ValidateCSRFToken", ctx, "sess", "csrf").Return(nil)
		mockAuth.grantPermissionsScoped([]auth.Permission{{Action: action, Resource: auth.ResourcePurchases}}, []string{accA})

		creator := "creator-uuid"
		mockStore.On("GetRIExchangeRecord", ctx, id).Return(&config.RIExchangeRecord{
			ID: id, Status: "pending", ApprovalToken: config.HashApprovalToken("tok"), SourceRIIDs: []string{"ri-1"},
			PaymentDue: "50.00", CreatedByUserID: &creator,
		}, nil).Maybe()
		mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{
			RIExchangeMaxDailyUSD: 1000, RIExchangeMaxPerExchangeUSD: 500,
		}, nil).Maybe()
		mockStore.On("TransitionRIExchangeStatus", ctx, id, "pending", "processing", mock.Anything).
			Return(&config.RIExchangeRecord{ID: id, Status: "processing", SourceRIIDs: []string{"ri-1"}, PaymentDue: "50.00"}, nil).Maybe()
		mockStore.On("ReserveApprovedRIExchange", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return("500.000000", nil).Maybe()
		mockStore.On("FailRIExchange", mock.Anything, id, mock.AnythingOfType("string")).Return(nil).Maybe()
		mockStore.On("StampRIExchangeApprovedBy", mock.Anything, id, mock.Anything).Return(nil).Maybe()

		return &Handler{
			config:                 mockStore,
			auth:                   mockAuth,
			reshapeAccountResolver: func(context.Context) (string, error) { return deploymentAccount, nil },
		}, mockStore
	}

	t.Run("out of scope is not found", func(t *testing.T) {
		h, mockStore := setup(t, accB, auth.ActionApproveAny)
		_, err := h.approveRIExchange(ctx, csrfSessionReq(), id, "")
		require.ErrorIs(t, err, errNotFound)
		mockStore.AssertNotCalled(t, "GetRIExchangeRecord", ctx, id)
		mockStore.AssertNotCalled(t, "TransitionRIExchangeStatus", ctx, id, "pending", "processing", mock.Anything)
	})

	t.Run("out of scope approve-own is not found", func(t *testing.T) {
		h, mockStore := setup(t, accB, auth.ActionApproveOwn)
		_, err := h.approveRIExchange(ctx, csrfSessionReq(), id, "")
		require.ErrorIs(t, err, errNotFound)
		mockStore.AssertNotCalled(t, "TransitionRIExchangeStatus", ctx, id, "pending", "processing", mock.Anything)
	})

	t.Run("out of scope with a valid token falls through to the token flow", func(t *testing.T) {
		h, mockStore := setup(t, accB, auth.ActionApproveAny)
		_, err := h.approveRIExchange(ctx, csrfSessionReq(), id, "tok")
		require.NoError(t, err)
		mockStore.AssertCalled(t, "TransitionRIExchangeStatus", ctx, id, "pending", "processing", mock.Anything)
	})

	t.Run("scope resolver error fails closed", func(t *testing.T) {
		h, mockStore := setup(t, accA, auth.ActionApproveAny)
		h.reshapeAccountResolver = func(context.Context) (string, error) { return "", errors.New("resolver down") }
		_, err := h.approveRIExchange(ctx, csrfSessionReq(), id, "")
		require.Error(t, err)
		require.NotErrorIs(t, err, errNotFound)
		mockStore.AssertNotCalled(t, "TransitionRIExchangeStatus", ctx, id, "pending", "processing", mock.Anything)
	})

	t.Run("in scope is approved", func(t *testing.T) {
		h, mockStore := setup(t, accA, auth.ActionApproveAny)
		_, err := h.approveRIExchange(ctx, csrfSessionReq(), id, "")
		require.NoError(t, err)
		mockStore.AssertCalled(t, "TransitionRIExchangeStatus", ctx, id, "pending", "processing", mock.Anything)
	})
}

type riApproverGrant func(m *MockAuthService, userID string)

func grantRIAdmin(m *MockAuthService, _ string) { m.grantAdminPurchaser() }

func grantRIApproveOwn(m *MockAuthService, userID string) {
	m.On("GetAllowedAccountsAPI", context.Background(), userID).Return(nil, nil).Maybe()
	m.On("HasPermissionAPI", context.Background(), userID, auth.ActionApproveAny, auth.ResourcePurchases).Return(false, nil)
	m.On("HasPermissionAPI", context.Background(), userID, auth.ActionApproveOwn, auth.ResourcePurchases).Return(true, nil)
}

func grantRINone(m *MockAuthService, userID string) {
	m.On("HasPermissionAPI", context.Background(), userID, mock.Anything, auth.ResourcePurchases).Return(false, nil)
}

// riApprovePath is how a fourEyesRIExchangeFixture request authenticates,
// which decides the auth calls the approval makes.
type riApprovePath int

const (
	// riApproveSession: session plus CSRF token, no email token.
	riApproveSession riApprovePath = iota
	// riApproveTokenWithSession: the email token, with a session that
	// attributes the approver. The token authorizes, so no CSRF check runs.
	riApproveTokenWithSession
	// riApproveTokenNoSession: the email token alone. No session is resolved
	// and no permission is consulted.
	riApproveTokenNoSession
)

// fourEyesRIExchangeFixture wires a pending exchange created by creatorID and a
// session for approverID holding the given approve grant. path and approved
// say which auth calls and which writes the approval must make; any write a
// refused approval attempts hits an unregistered method and fails the test.
func fourEyesRIExchangeFixture(t *testing.T, creatorID *string, approverID string, grant riApproverGrant, fourEyes bool, path riApprovePath, approved bool) (*Handler, *MockConfigStore, string, string) {
	t.Helper()
	ctx := context.Background()
	id := "550e8400-e29b-41d4-a716-446655440021"
	token := "four-eyes-token"
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	if path != riApproveTokenNoSession {
		mockAuth.On("ValidateSession", ctx, "sess").Return(&Session{UserID: approverID, Email: approverID + "@example.com"}, nil)
		grant(mockAuth, approverID)
	}
	if path == riApproveSession {
		mockAuth.On("ValidateCSRFToken", ctx, "sess", "csrf").Return(nil)
	}

	mockStore.On("GetRIExchangeRecord", ctx, id).Return(&config.RIExchangeRecord{
		ID:              id,
		Status:          "pending",
		ApprovalToken:   config.HashApprovalToken(token),
		SourceRIIDs:     []string{"ri-1"},
		PaymentDue:      "50.00",
		CreatedByUserID: creatorID,
	}, nil)
	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{
		RequireDifferentApprover:    fourEyes,
		RIExchangeMaxDailyUSD:       1000,
		RIExchangeMaxPerExchangeUSD: 500,
	}, nil)
	if approved {
		// The claimed record carries no region, so execution fails closed at
		// the region guard before reserving the spend ceiling: the approval
		// ends in FailRIExchange, and is stamped only when a session named
		// the approver.
		mockStore.On("TransitionRIExchangeStatus", ctx, id, "pending", "processing", mock.Anything).
			Return(&config.RIExchangeRecord{ID: id, Status: "processing", SourceRIIDs: []string{"ri-1"}, PaymentDue: "50.00"}, nil)
		mockStore.On("FailRIExchange", mock.Anything, id, mock.AnythingOfType("string")).Return(nil)
		if path != riApproveTokenNoSession {
			mockStore.On("StampRIExchangeApprovedBy", mock.Anything, id, mock.Anything).Return(nil)
		}
	}

	return &Handler{config: mockStore, auth: mockAuth}, mockStore, id, token
}

func csrfSessionReq() *events.LambdaFunctionURLRequest {
	req := sessionReq("sess")
	req.Headers["x-csrf-token"] = "csrf"
	return req
}

// TestApproveRIExchange_FourEyes (issue #221): with 4-eyes on, the creator cannot
// approve their own exchange through the session or the email-token branch.
func TestApproveRIExchange_FourEyes(t *testing.T) {
	ctx := context.Background()
	creator := "creator-uuid"

	denied := func(t *testing.T, mockStore *MockConfigStore, err error, code int) {
		t.Helper()
		require.Error(t, err)
		ce, ok := IsClientError(err)
		require.True(t, ok, "want ClientError, got %v", err)
		assert.Equal(t, code, ce.code)
		mockStore.AssertNotCalled(t, "TransitionRIExchangeStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	}
	approved := func(t *testing.T, mockStore *MockConfigStore, err error) {
		t.Helper()
		require.NoError(t, err)
		mockStore.AssertCalled(t, "TransitionRIExchangeStatus", ctx, mock.Anything, "pending", "processing", mock.Anything)
	}

	t.Run("session: creator with approve-own denied", func(t *testing.T) {
		h, store, id, _ := fourEyesRIExchangeFixture(t, &creator, creator, grantRIApproveOwn, true, riApproveSession, false)
		_, err := h.approveRIExchange(ctx, csrfSessionReq(), id, "")
		denied(t, store, err, 403)
		assert.Contains(t, err.Error(), "4-eyes mode")
	})

	t.Run("session: admin creator denied", func(t *testing.T) {
		h, store, id, _ := fourEyesRIExchangeFixture(t, &creator, creator, grantRIAdmin, true, riApproveSession, false)
		_, err := h.approveRIExchange(ctx, csrfSessionReq(), id, "")
		denied(t, store, err, 403)
	})

	t.Run("session: creator holding the email token still denied", func(t *testing.T) {
		h, store, id, token := fourEyesRIExchangeFixture(t, &creator, creator, grantRIApproveOwn, true, riApproveSession, false)
		_, err := h.approveRIExchange(ctx, csrfSessionReq(), id, token)
		denied(t, store, err, 403)
	})

	t.Run("token: creator session without approve rights denied", func(t *testing.T) {
		h, store, id, token := fourEyesRIExchangeFixture(t, &creator, creator, grantRINone, true, riApproveTokenWithSession, false)
		_, err := h.approveRIExchange(ctx, sessionReq("sess"), id, token)
		denied(t, store, err, 403)
		assert.Contains(t, err.Error(), "4-eyes mode")
	})

	t.Run("token: no session fails closed", func(t *testing.T) {
		h, store, id, token := fourEyesRIExchangeFixture(t, &creator, creator, grantRIAdmin, true, riApproveTokenNoSession, false)
		_, err := h.approveRIExchange(ctx, &events.LambdaFunctionURLRequest{}, id, token)
		denied(t, store, err, 500)
	})

	t.Run("session: different approver allowed", func(t *testing.T) {
		h, store, id, _ := fourEyesRIExchangeFixture(t, &creator, "approver-uuid", grantRIAdmin, true, riApproveSession, true)
		_, err := h.approveRIExchange(ctx, csrfSessionReq(), id, "")
		approved(t, store, err)
	})

	t.Run("token: different approver allowed", func(t *testing.T) {
		h, store, id, token := fourEyesRIExchangeFixture(t, &creator, "approver-uuid", grantRINone, true, riApproveTokenWithSession, true)
		_, err := h.approveRIExchange(ctx, sessionReq("sess"), id, token)
		approved(t, store, err)
	})

	t.Run("mode off: creator allowed", func(t *testing.T) {
		h, store, id, _ := fourEyesRIExchangeFixture(t, &creator, creator, grantRIApproveOwn, false, riApproveSession, true)
		_, err := h.approveRIExchange(ctx, csrfSessionReq(), id, "")
		approved(t, store, err)
	})

	t.Run("automated exchange without creator stays approvable", func(t *testing.T) {
		h, store, id, _ := fourEyesRIExchangeFixture(t, nil, "approver-uuid", grantRIAdmin, true, riApproveSession, true)
		_, err := h.approveRIExchange(ctx, csrfSessionReq(), id, "")
		approved(t, store, err)
	})

	// Issue #518: every production exchange has no creator, so the creator-less
	// branch is the one that runs. Under 4-eyes it must still demand an
	// identified approver instead of letting an anonymous token holder through.
	t.Run("automated exchange: token without session fails closed", func(t *testing.T) {
		h, store, id, token := fourEyesRIExchangeFixture(t, nil, "approver-uuid", grantRIAdmin, true, riApproveTokenNoSession, false)
		_, err := h.approveRIExchange(ctx, &events.LambdaFunctionURLRequest{}, id, token)
		denied(t, store, err, 500)
		assert.Contains(t, err.Error(), "no session could be resolved")
		store.AssertNotCalled(t, "StampRIExchangeApprovedBy", mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("automated exchange: token with a resolved session allowed", func(t *testing.T) {
		h, store, id, token := fourEyesRIExchangeFixture(t, nil, "approver-uuid", grantRINone, true, riApproveTokenWithSession, true)
		_, err := h.approveRIExchange(ctx, sessionReq("sess"), id, token)
		approved(t, store, err)
	})

	t.Run("automated exchange: mode off, token without session allowed", func(t *testing.T) {
		h, store, id, token := fourEyesRIExchangeFixture(t, nil, "approver-uuid", grantRIAdmin, false, riApproveTokenNoSession, true)
		_, err := h.approveRIExchange(ctx, &events.LambdaFunctionURLRequest{}, id, token)
		approved(t, store, err)
	})
}

// TestRequireDifferentRIExchangeApprover_ConfigLoadFailsClosed (issue #518): a
// creator-less exchange approved with no session must not be let through when
// the 4-eyes setting cannot be read.
func TestRequireDifferentRIExchangeApprover_ConfigLoadFailsClosed(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	t.Cleanup(func() { store.AssertExpectations(t) })
	store.On("GetGlobalConfig", ctx).Return(nil, errors.New("db down"))
	h := &Handler{config: store}
	err := h.requireDifferentRIExchangeApprover(ctx, nil, &config.RIExchangeRecord{ID: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to load global config")
}

// TestApproveRIExchange_TokenPathAttributesResolvedApprover (issue #518): a token
// approval whose deep link resolves a session records that user as the transition
// actor and stamps approved_by; with no session both stay unset.
func TestApproveRIExchange_TokenPathAttributesResolvedApprover(t *testing.T) {
	ctx := context.Background()
	approver := "11111111-1111-4111-8111-111111111111"

	t.Run("session resolves", func(t *testing.T) {
		h, store, id, token := fourEyesRIExchangeFixture(t, nil, approver, grantRINone, false, riApproveTokenWithSession, true)
		_, err := h.approveRIExchange(ctx, sessionReq("sess"), id, token)
		require.NoError(t, err)
		store.AssertCalled(t, "TransitionRIExchangeStatus", ctx, id, "pending", "processing", &approver)
		store.AssertCalled(t, "StampRIExchangeApprovedBy", mock.Anything, id, approver+"@example.com")
	})

	t.Run("no session leaves the actor unset", func(t *testing.T) {
		h, store, id, token := fourEyesRIExchangeFixture(t, nil, approver, grantRINone, false, riApproveTokenNoSession, true)
		_, err := h.approveRIExchange(ctx, &events.LambdaFunctionURLRequest{}, id, token)
		require.NoError(t, err)
		store.AssertCalled(t, "TransitionRIExchangeStatus", ctx, id, "pending", "processing", (*string)(nil))
		store.AssertNotCalled(t, "StampRIExchangeApprovedBy", mock.Anything, mock.Anything, mock.Anything)
	})
}

// TestApproveRIExchange_TokenPathSkipsStampWhenExecutionErrors (issue #518): when
// execution returns an error (money moved, ledger write failed), the transition
// still records the actor but approved_by is not stamped, as on the session path.
func TestApproveRIExchange_TokenPathSkipsStampWhenExecutionErrors(t *testing.T) {
	ctx := context.Background()
	approver := "11111111-1111-4111-8111-111111111111"
	id := "550e8400-e29b-41d4-a716-446655440021"
	token := "four-eyes-token"

	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("ValidateSession", ctx, "sess").Return(&Session{UserID: approver, Email: "approver@example.com"}, nil)
	grantRINone(mockAuth, approver)
	mockStore.On("GetRIExchangeRecord", ctx, id).Return(&config.RIExchangeRecord{
		ID:               id,
		Status:           "pending",
		ApprovalToken:    config.HashApprovalToken(token),
		Region:           "us-east-1",
		SourceRIIDs:      []string{"ri-1"},
		TargetOfferingID: "offering-1",
		TargetCount:      1,
		PaymentDue:       "0",
	}, nil)
	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{
		RIExchangeMaxDailyUSD:       1000,
		RIExchangeMaxPerExchangeUSD: 500,
	}, nil)
	mockStore.On("ReserveApprovedRIExchange", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return("500.000000", nil)
	mockStore.On("TransitionRIExchangeStatus", ctx, id, "pending", "processing", &approver).
		Return(&config.RIExchangeRecord{ID: id, Status: "processing"}, nil)
	mockStore.On("CompleteRIExchangeWithPayment", mock.Anything, id, "exch-518", "0").
		Return(fmt.Errorf("DB write failed")).Times(3)

	h := &Handler{
		config: mockStore,
		auth:   mockAuth,
		executeExchangeFn: func(_ context.Context, _ exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
			return "exch-518", &exchange.ExchangeQuoteSummary{PaymentDueUSDStr: "0"}, nil
		},
	}

	_, err := h.approveRIExchange(ctx, sessionReq("sess"), id, token)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ledger update failed")
	mockStore.AssertCalled(t, "TransitionRIExchangeStatus", ctx, id, "pending", "processing", &approver)
	mockStore.AssertNotCalled(t, "StampRIExchangeApprovedBy", mock.Anything, mock.Anything, mock.Anything)
}

// TestApproveRIExchange_LegacyTokenStillWorks verifies that the token-only path
// continues to work for non-session callers after the dual-auth refactor (backwards-compat).
func TestApproveRIExchange_LegacyTokenStillWorks(t *testing.T) {
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	h := &Handler{config: mockStore} // no auth configured -> tryGetSession returns nil
	ctx := context.Background()
	id := "550e8400-e29b-41d4-a716-446655440012"
	token := "legacy-token"

	mockStore.On("GetRIExchangeRecord", ctx, id).Return(&config.RIExchangeRecord{
		ID:            id,
		ApprovalToken: config.HashApprovalToken(token),
		Status:        "pending",
		SourceRIIDs:   []string{"ri-1"},
		PaymentDue:    "10.00",
	}, nil)
	mockStore.On("TransitionRIExchangeStatus", ctx, id, "pending", "processing", mock.Anything).
		Return(&config.RIExchangeRecord{ID: id, Status: "processing"}, nil)
	// The record carries no region, so execution fails closed at the region
	// guard before ReserveApprovedRIExchange: the approval ends in FailRIExchange.
	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{
		RIExchangeMaxDailyUSD:       1000,
		RIExchangeMaxPerExchangeUSD: 500,
	}, nil)
	mockStore.On("FailRIExchange", mock.Anything, id, mock.AnythingOfType("string")).Return(nil)

	req := &events.LambdaFunctionURLRequest{}
	_, err := h.approveRIExchange(ctx, req, id, token)
	require.NoError(t, err)
	mockStore.AssertCalled(t, "TransitionRIExchangeStatus", ctx, id, "pending", "processing", mock.Anything)
}

func TestRejectRIExchange_MissingToken(t *testing.T) {
	h := &Handler{}
	ctx := context.Background()
	id := "550e8400-e29b-41d4-a716-446655440004"

	_, err := h.rejectRIExchange(ctx, id, "")
	assert.Error(t, err)
	ce, ok := IsClientError(err)
	assert.True(t, ok)
	assert.Equal(t, 400, ce.code)
	assert.Contains(t, err.Error(), "rejection token is required")
}

// TestRejectRIExchange_EmptyStoredToken is a regression test for issue #399.
// A record with an empty ApprovalToken must be rejected with 403 rather than
// being canceled by any caller passing an empty token string, because
// crypto/subtle.ConstantTimeCompare([]byte(""), []byte("")) == 1.
func TestRejectRIExchange_EmptyStoredToken(t *testing.T) {
	mockStore := new(MockConfigStore)
	h := &Handler{config: mockStore}
	ctx := context.Background()
	id := "550e8400-e29b-41d4-a716-446655440005"

	// Record exists but was persisted without an ApprovalToken.
	mockStore.On("GetRIExchangeRecord", ctx, id).Return(&config.RIExchangeRecord{
		ID:            id,
		ApprovalToken: "", // empty stored token
		Status:        "pending",
	}, nil)

	// Passing an empty token must NOT match the empty stored token.
	_, err := h.rejectRIExchange(ctx, id, "sometoken")
	assert.Error(t, err)
	ce, ok := IsClientError(err)
	assert.True(t, ok)
	assert.Equal(t, 403, ce.code)
	// Same answer as a wrong token or a missing record (issue #601).
	assert.Contains(t, err.Error(), "invalid rejection token")

	mockStore.AssertExpectations(t)
}

// fakeReshapeEC2Stub is a unit-test stub for reshapeEC2Client.
// Returns a single convertible RI so the downstream
// AnalyzeReshapingWithRecs actually invokes purchaseRecLookupFromStore's
// closure — without an RI in the input slice the recs lookup is never
// called and the region-normalization assertion would never fire
// (silent test trap). Mirrors the integration-test fake but lives
// here so this unit test runs without the //go:build integration tag.
//
// offered / offeredErr drive GetValidResourceTypes (the region's
// instance type catalog); offeredCalls counts the catalog lookups.
type fakeReshapeEC2Stub struct {
	instances    []ec2svc.ConvertibleRI
	offered      []string
	offeredErr   error
	offeredCalls int
}

func (f *fakeReshapeEC2Stub) ListConvertibleReservedInstances(_ context.Context) ([]ec2svc.ConvertibleRI, error) {
	return f.instances, nil
}

func (f *fakeReshapeEC2Stub) GetValidResourceTypes(_ context.Context) ([]string, error) {
	f.offeredCalls++
	return f.offered, f.offeredErr
}

// fakeReshapeRecsStub returns a configurable RI utilization slice so
// the reshape pipeline runs without a real Cost Explorer dependency.
// AnalyzeReshapingWithRecs only emits a recommendation (and only then
// invokes the recs lookup closure) when an RI has a utilization entry
// BELOW the threshold — without one the analyzer returns nil and the
// closure never fires, producing a silent test. Seed with util-percent
// well under the request's threshold (95) to guarantee the closure
// runs.
type fakeReshapeRecsStub struct {
	utilization []recommendations.RIUtilization
}

func (f *fakeReshapeRecsStub) GetRIUtilization(_ context.Context, _ int, _ string) ([]recommendations.RIUtilization, error) {
	return f.utilization, nil
}

// TestGetReshapeRecommendations_EmptyRegionUsesConfigRegion pins the
// region-normalization fix from commit afc3aa1ff: when the caller
// omits ?region= (or sends ?region=), the handler MUST adopt
// cfg.Region (the value the AWS SDK clients are actually talking to)
// before threading the region through to the recs lookup. Without
// the normalization the recs query lands as Region:"" — an unscoped
// read that surfaces alternatives from every region onto the reshape
// page.
//
// Mock injection strategy: build a Handler with the same factory-
// injection seams the integration test uses (reshapeEC2Factory,
// reshapeRecsFactory, reshapeAccountResolver) so the request runs
// without real AWS calls or Postgres. Pre-seal awsCfgOnce with
// Region:"us-west-2" so loadAWSConfigWithRegion(ctx, "") returns a
// config carrying that region. Wire MockConfigStore.ListStoredRecommendations
// with mock.Run to capture every filter the closure passes — the
// load-bearing assertion is filter.Region == "us-west-2" (not "").
//
// Reverting the `if region == "" { region = cfg.Region }` block from
// handler_ri_exchange.go makes this test fail loudly with the captured
// filter showing Region:"".
func TestGetReshapeRecommendations_EmptyRegionUsesConfigRegion(t *testing.T) {
	const cfgRegion = "us-west-2"

	mockStore := &MockConfigStore{}
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	// mock.Run captures every invocation's arguments without affecting
	// the return value. Returning nil/nil from ListStoredRecommendations
	// means "no recs in this region" which the downstream pipeline
	// treats as empty alternatives — fine for our purposes.
	var capturedFilters []config.RecommendationFilter
	mockStore.On("ListStoredRecommendations", mock.Anything, mock.Anything).
		Return([]config.RecommendationRecord(nil), nil).
		Run(func(args mock.Arguments) {
			capturedFilters = append(capturedFilters, args.Get(1).(config.RecommendationFilter))
		})

	h := &Handler{
		config: mockStore,
		auth:   &mockAuthForExchange{},
		// Inject one convertible RI so AnalyzeReshapingWithRecs
		// actually drives purchaseRecLookupFromStore — otherwise the
		// closure never fires and the assertion is silent.
		reshapeEC2Factory: func(_ aws.Config) reshapeEC2Client {
			return &fakeReshapeEC2Stub{
				instances: []ec2svc.ConvertibleRI{
					{ReservedInstanceID: "ri-1", InstanceType: "m5.xlarge", InstanceCount: 1, CurrencyCode: "USD", Duration: 31536000, FixedPrice: 876},
				},
			}
		},
		reshapeRecsFactory: func(_ aws.Config) reshapeRecsClient {
			// Util well below threshold (95) → analyzeRI emits a
			// recommendation → AnalyzeReshapingWithRecs invokes the
			// recs lookup closure → MockConfigStore.ListStoredRecommendations
			// captures the filter the assertion below inspects.
			return &fakeReshapeRecsStub{
				utilization: []recommendations.RIUtilization{
					{ReservedInstanceID: "ri-1", UtilizationPercent: 50.0},
				},
			}
		},
		// Bypass STS so the test is hermetic. Empty cloud-account ID
		// is the legitimate "no scope filter" path; this test isolates
		// the region-normalization regression, not account scoping.
		reshapeAccountResolver: func(_ context.Context) (string, error) { return "", nil },
	}
	// Pre-seal awsCfgOnce with the cfg.Region the handler must adopt
	// when the caller sends an empty ?region=. loadAWSConfigWithRegion
	// returns this config unmodified (region override is skipped on
	// empty input), so cfg.Region == cfgRegion downstream.
	h.awsCfgOnce.Do(func() {
		h.awsCfg = aws.Config{Region: cfgRegion}
	})

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer test-token"},
		QueryStringParameters: map[string]string{
			// Explicit empty value — the regression case from issue #151.
			// Equivalent to omitting the key entirely (Go map zero-value).
			"region":        "",
			"lookback_days": "30",
			"threshold":     "95.0",
		},
	}

	_, err := h.getReshapeRecommendations(context.Background(), req)
	require.NoError(t, err, "reshape handler must succeed when region is empty (cfg.Region used)")

	// Guard against a silent test: if AnalyzeReshapingWithRecs ever
	// stops invoking the closure for synthetic RIs, the region check
	// would vacuously pass. Require at least one captured filter.
	require.NotEmpty(t, capturedFilters,
		"recs lookup must be invoked at least once — without it the region-normalization "+
			"assertion would silently pass even if the fix were reverted")

	// Load-bearing assertion: every captured filter must carry the
	// normalized cfg.Region value, NOT the empty string the request
	// arrived with. If the `if region == "" { region = cfg.Region }`
	// block in getReshapeRecommendations is removed, this fails.
	for i, f := range capturedFilters {
		assert.Equal(t, cfgRegion, f.Region,
			"capturedFilters[%d].Region must equal cfg.Region (%q), not %q — empty ?region= "+
				"must normalize to cfg.Region or the recs lookup runs unscoped and leaks "+
				"alternatives from other regions onto the reshape page (commit afc3aa1ff)",
			i, cfgRegion, f.Region)
	}
}

// Alternatives carry the RIs' own currency; RIs reporting none yield no
// currency_code, never a fabricated "USD" (#361).
func TestGetReshapeRecommendations_AlternativeCurrencyFromRIs(t *testing.T) {
	cases := []struct {
		name       string
		riCurrency string
	}{
		{name: "RIs without currency", riCurrency: ""},
		{name: "JPY RIs", riCurrency: "JPY"},
		{name: "EUR RIs", riCurrency: "EUR"},
		{name: "USD RIs", riCurrency: "USD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mockStore := &MockConfigStore{}
			t.Cleanup(func() { mockStore.AssertExpectations(t) })
			mockStore.On("ListStoredRecommendations", mock.Anything, mock.Anything).
				Return([]config.RecommendationRecord{
					{ID: "rec-1", ResourceType: "c6i.large", Count: 1, MonthlyCost: aws.Float64(40)},
				}, nil)
			h := &Handler{
				config: mockStore,
				auth:   &mockAuthForExchange{},
				reshapeEC2Factory: func(_ aws.Config) reshapeEC2Client {
					// FixedPrice 100 over 1y is ~8.33/month; NF 8 x 8.33 stays
					// under the c6i.large rec's NF 4 x 40, so the dollar-units
					// gate keeps the alternative this test is about.
					return &fakeReshapeEC2Stub{instances: []ec2svc.ConvertibleRI{
						{ReservedInstanceID: "ri-1", InstanceType: "m5.xlarge", InstanceCount: 1, CurrencyCode: tc.riCurrency, Duration: 31536000, FixedPrice: 100},
					}}
				},
				reshapeRecsFactory: func(_ aws.Config) reshapeRecsClient {
					return &fakeReshapeRecsStub{utilization: []recommendations.RIUtilization{
						{ReservedInstanceID: "ri-1", UtilizationPercent: 50.0},
					}}
				},
				reshapeAccountResolver: func(_ context.Context) (string, error) { return "", nil },
			}
			h.awsCfgOnce.Do(func() { h.awsCfg = aws.Config{Region: "us-east-1"} })

			req := &events.LambdaFunctionURLRequest{
				Headers:               map[string]string{"authorization": "Bearer test-token"},
				QueryStringParameters: map[string]string{"lookback_days": "30", "threshold": "95.0"},
			}
			out, err := h.getReshapeRecommendations(context.Background(), req)
			require.NoError(t, err)
			resp, ok := out.(*ReshapeRecommendationsResponse)
			require.True(t, ok)
			require.Len(t, resp.Recommendations, 1)
			alts := resp.Recommendations[0].AlternativeTargets
			require.Len(t, alts, 1, "the c6i.large rec must surface as a cross-family alternative")
			assert.Equal(t, tc.riCurrency, alts[0].CurrencyCode)

			raw, err := json.Marshal(alts[0])
			require.NoError(t, err)
			assert.Equal(t, tc.riCurrency != "", bytes.Contains(raw, []byte(`"currency_code"`)),
				"currency_code must be present only when the RIs report a currency: %s", raw)
		})
	}
}

// Suppress unused import warnings.
var _ = mock.Anything
var _ = time.Now
var _ = config.RIExchangeRecord{}

// --- Azure exchangeable RI tests ---

// stubAzureExchangeClient is a minimal implementation of azureExchangeClient
// for unit tests. It only exercises ListExchangeableReservations; the
// CalculateExchange/ExecuteExchange methods exist solely to satisfy the
// widened interface and are not used by the listing tests below (the
// dedicated mockAzureExchangeOpsClient in this file covers those).
type stubAzureExchangeClient struct {
	err          error
	reservations []azurecompute.ExchangeableReservation
}

func (s *stubAzureExchangeClient) ListExchangeableReservations(_ context.Context) ([]azurecompute.ExchangeableReservation, error) {
	return s.reservations, s.err
}

func (s *stubAzureExchangeClient) CalculateExchange(_ context.Context, _ []azurecompute.ExchangeableReservation, _ []azurecompute.ExchangeTarget) (*azurecompute.ExchangePreview, []azurecompute.CompatibleOffering, error) {
	return nil, nil, fmt.Errorf("stubAzureExchangeClient.CalculateExchange not implemented")
}

func (s *stubAzureExchangeClient) ExecuteExchange(_ context.Context, _ string) (*azurecompute.ExchangeResult, error) {
	return nil, fmt.Errorf("stubAzureExchangeClient.ExecuteExchange not implemented")
}

func TestListExchangeableAzureRIs_RequiresPermission(t *testing.T) {
	h := &Handler{} // no auth configured
	_, err := h.listExchangeableAzureRIs(context.Background(), &events.LambdaFunctionURLRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "authentication")
}

func TestListExchangeableAzureRIs_EmptyList(t *testing.T) {
	stub := &stubAzureExchangeClient{reservations: []azurecompute.ExchangeableReservation{}}
	h := &Handler{
		auth: &mockAuthForExchange{},
		azureExchangeFactory: func(_ string) azureExchangeClient {
			return stub
		},
	}

	res, err := h.listExchangeableAzureRIs(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer test-token"},
	})
	require.NoError(t, err)
	resp, ok := res.(*ExchangeableAzureRIsResponse)
	require.True(t, ok)
	assert.Empty(t, resp.Reservations)
}

func TestListExchangeableAzureRIs_PopulatedList(t *testing.T) {
	want := []azurecompute.ExchangeableReservation{
		{
			ReservationOrderID:  "order-1111",
			ReservationID:       "/providers/Microsoft.Capacity/reservationOrders/order-1111/reservations/res-aaaa",
			SKU:                 "Standard_D2s_v3",
			Quantity:            2,
			Region:              "eastus",
			Term:                "P1Y",
			InstanceFlexibility: "On",
			DisplayName:         "my-reservation",
		},
	}
	stub := &stubAzureExchangeClient{reservations: want}
	h := &Handler{
		auth: &mockAuthForExchange{},
		azureExchangeFactory: func(_ string) azureExchangeClient {
			return stub
		},
	}

	res, err := h.listExchangeableAzureRIs(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer test-token"},
	})
	require.NoError(t, err)
	resp, ok := res.(*ExchangeableAzureRIsResponse)
	require.True(t, ok)
	require.Len(t, resp.Reservations, 1)
	r := resp.Reservations[0]
	assert.Equal(t, "order-1111", r.ReservationOrderID)
	assert.Equal(t, "Standard_D2s_v3", r.SKU)
	assert.Equal(t, int32(2), r.Quantity)
	assert.Equal(t, "eastus", r.Region)
	assert.Equal(t, "P1Y", r.Term)
	assert.Equal(t, "On", r.InstanceFlexibility)
}

func TestListExchangeableAzureRIs_ClientError(t *testing.T) {
	stub := &stubAzureExchangeClient{err: fmt.Errorf("azure api unavailable")}
	h := &Handler{
		auth: &mockAuthForExchange{},
		azureExchangeFactory: func(_ string) azureExchangeClient {
			return stub
		},
	}

	_, err := h.listExchangeableAzureRIs(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer test-token"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "azure api unavailable")
}

func TestListExchangeableAzureRIs_SubscriptionIDPassedToFactory(t *testing.T) {
	var capturedSubID string
	stub := &stubAzureExchangeClient{reservations: []azurecompute.ExchangeableReservation{}}
	h := &Handler{
		auth: &mockAuthForExchange{},
		azureExchangeFactory: func(subID string) azureExchangeClient {
			capturedSubID = subID
			return stub
		},
	}

	_, err := h.listExchangeableAzureRIs(context.Background(), &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"authorization": "Bearer test-token"},
		QueryStringParameters: map[string]string{"subscription_id": "sub-abc"},
	})
	require.NoError(t, err)
	assert.Equal(t, "sub-abc", capturedSubID)
}

// --- allowed_accounts scoping on the tenant-wide listing (issue #1656) ---
//
// ListExchangeableReservations returns every reservation the deployment's
// Azure credential can read, tenant-wide. Before this fix, a caller with
// "view:purchases" saw every subscription's reservations regardless of their
// own allowed_accounts scope. These tests exercise the fix directly: a
// scoped session must see only the rows billed to a subscription it is
// scoped to, both with and without ?subscription_id=.

// TestListExchangeableAzureRIs_ScopedFiltersToOwnSubscription is the core
// regression: a session scoped to account A's subscription, calling the
// listing with NO subscription_id filter, must receive only account A's
// reservation. Account B's reservation -- present in the same tenant-wide
// listing -- must not appear anywhere in the response.
func TestListExchangeableAzureRIs_ScopedFiltersToOwnSubscription(t *testing.T) {
	ctx := context.Background()
	mockAuth := scopedAzureAuth(t, "view", "purchases", []string{"acct-a"})
	store := &MockConfigStore{}
	azureListed := false
	store.ListCloudAccountsFn = func(_ context.Context, filter config.CloudAccountFilter) ([]config.CloudAccount, error) {
		// getAccountScope loads every registered account (no filter) to find
		// which display names are unique (#433); the handler's own lookup
		// must narrow to azure.
		if filter.Provider != nil {
			require.Equal(t, "azure", *filter.Provider)
			azureListed = true
		}
		return []config.CloudAccount{
			{ID: "acct-a", Name: "A Team", Provider: "azure", ExternalID: "sub-a"},
			{ID: "acct-b", Name: "B Team", Provider: "azure", ExternalID: "sub-b"},
		}, nil
	}
	t.Cleanup(func() { assert.True(t, azureListed, "the scope filter must narrow to azure accounts") })
	stub := &stubAzureExchangeClient{reservations: []azurecompute.ExchangeableReservation{
		{ReservationID: "res-a", BillingScopeID: "/subscriptions/sub-a", SKU: "Standard_D2s_v3", Quantity: 1, Region: "eastus"},
		{ReservationID: "res-b", BillingScopeID: "/subscriptions/sub-b", SKU: "Standard_D4s_v3", Quantity: 3, Region: "westeurope"},
	}}

	h := &Handler{
		auth:                 mockAuth,
		config:               store,
		azureExchangeFactory: func(_ string) azureExchangeClient { return stub },
	}

	res, err := h.listExchangeableAzureRIs(ctx, &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer tok"},
	})
	require.NoError(t, err)
	resp, ok := res.(*ExchangeableAzureRIsResponse)
	require.True(t, ok)
	require.Len(t, resp.Reservations, 1, "only the scoped subscription's reservation must be returned")
	assert.Equal(t, "res-a", resp.Reservations[0].ReservationID)
	for _, r := range resp.Reservations {
		assert.NotEqual(t, "res-b", r.ReservationID, "account B's reservation must never appear in a scoped response")
		assert.NotContains(t, r.BillingScopeID, "sub-b", "account B's billing scope must not leak into a scoped response")
	}
}

// TestListExchangeableAzureRIs_ScopedEmptyCasesIndistinguishable asserts
// that a scoped caller cannot tell "my subscriptions genuinely have zero
// exchangeable reservations" apart from "the tenant-wide listing had rows,
// but none could be attributed to a registered account I'm scoped to" --
// both must render as the same empty list, per the doc comment on
// listExchangeableAzureRIs: an unresolvable scope must never degrade to
// "show everything", and must not leak its distinctness either.
func TestListExchangeableAzureRIs_ScopedEmptyCasesIndistinguishable(t *testing.T) {
	ctx := context.Background()

	// Case 1: the scoped account (A) is registered, but the only tenant-wide
	// reservation belongs to a different, unscoped subscription (B). After
	// filtering, account A itself simply has zero reservations.
	zeroOwnAuth := scopedAzureAuth(t, "view", "purchases", []string{"acct-a"})
	zeroOwnStore := &MockConfigStore{}
	zeroOwnStore.ListCloudAccountsFn = func(_ context.Context, _ config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return []config.CloudAccount{
			{ID: "acct-a", Name: "A Team", Provider: "azure", ExternalID: "sub-a"},
			{ID: "acct-b", Name: "B Team", Provider: "azure", ExternalID: "sub-b"},
		}, nil
	}
	zeroOwnStub := &stubAzureExchangeClient{reservations: []azurecompute.ExchangeableReservation{
		{ReservationID: "res-b", BillingScopeID: "/subscriptions/sub-b", SKU: "Standard_D4s_v3", Quantity: 1, Region: "eastus"},
	}}
	hZeroOwn := &Handler{
		auth:                 zeroOwnAuth,
		config:               zeroOwnStore,
		azureExchangeFactory: func(_ string) azureExchangeClient { return zeroOwnStub },
	}
	zeroOwnRes, zeroOwnErr := hZeroOwn.listExchangeableAzureRIs(ctx, &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer tok"},
	})

	// Case 2: the scoped account (A) is registered, and the tenant-wide
	// listing has a row, but that row cannot be attributed to ANY registered
	// CloudAccount (no matching BillingScopeID) -- e.g. a reservation billed
	// to a subscription that was never onboarded.
	unattributedAuth := scopedAzureAuth(t, "view", "purchases", []string{"acct-a"})
	unattributedStore := &MockConfigStore{}
	unattributedStore.ListCloudAccountsFn = func(_ context.Context, _ config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return []config.CloudAccount{
			{ID: "acct-a", Name: "A Team", Provider: "azure", ExternalID: "sub-a"},
		}, nil
	}
	unattributedStub := &stubAzureExchangeClient{reservations: []azurecompute.ExchangeableReservation{
		{ReservationID: "res-orphan", BillingScopeID: "/subscriptions/sub-unregistered", SKU: "Standard_D4s_v3", Quantity: 1, Region: "eastus"},
	}}
	hUnattributed := &Handler{
		auth:                 unattributedAuth,
		config:               unattributedStore,
		azureExchangeFactory: func(_ string) azureExchangeClient { return unattributedStub },
	}
	unattributedRes, unattributedErr := hUnattributed.listExchangeableAzureRIs(ctx, &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer tok"},
	})

	require.NoError(t, zeroOwnErr)
	require.NoError(t, unattributedErr)
	zeroOwnResp, ok := zeroOwnRes.(*ExchangeableAzureRIsResponse)
	require.True(t, ok)
	unattributedResp, ok := unattributedRes.(*ExchangeableAzureRIsResponse)
	require.True(t, ok)
	assert.Empty(t, zeroOwnResp.Reservations)
	assert.Empty(t, unattributedResp.Reservations)
	assert.Equal(t, zeroOwnResp.Reservations, unattributedResp.Reservations,
		"a scoped caller with genuinely zero reservations and one whose rows could not be attributed must render identically")
}

// TestListExchangeableAzureRIs_SubscriptionIDOutOfScope asserts the
// ?subscription_id= path returns the same not-found shape (errNotFound) the
// POST siblings (getAzureCompatibleOfferings, executeAzureExchange) use for
// an out-of-scope subscription, so a scoped caller cannot use the listing
// endpoint to probe which subscriptions exist outside their scope.
func TestListExchangeableAzureRIs_SubscriptionIDOutOfScope(t *testing.T) {
	ctx := context.Background()
	opsClient := new(mockAzureExchangeOpsClient)
	ownsAzureSource(opsClient) // Maybe(): must never actually be called
	t.Cleanup(func() { opsClient.AssertExpectations(t) })

	h := &Handler{
		auth:                 scopedAzureAuth(t, "view", "purchases", []string{"acct-mine"}),
		config:               scopedAzureStore(), // only registered account is acct-other
		azureExchangeFactory: func(_ string) azureExchangeClient { return opsClient },
	}

	_, err := h.listExchangeableAzureRIs(ctx, &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"authorization": "Bearer tok"},
		QueryStringParameters: map[string]string{"subscription_id": "sub-1"},
	})
	require.Error(t, err, "a scoped session must not list an out-of-scope subscription's reservations")
	assert.ErrorIs(t, err, errNotFound, "must be the generic scope-check 404, matching the POST siblings")
	// ownsAzureSource registers ListExchangeableReservations with .Maybe(), so
	// AssertExpectations above passes whether it was called or not; assert it
	// explicitly to prove the scope gate refuses the request before the
	// tenant-wide listing is ever fetched.
	opsClient.AssertNotCalled(t, "ListExchangeableReservations", mock.Anything)
}

// TestListExchangeableAzureRIs_SubscriptionIDFiltersToOwnRows exercises
// filterAzureReservationsBySubscription directly through the handler -- the
// row-level filter on the ?subscription_id= path, which is the only filter
// this listing's production callers can actually reach (buildAzureExchangeClient
// resolves subscriptionID via an exact external_id match, so the
// no-subscription_id branch that reaches filterAzureReservationsByScope never
// gets a live Azure client in production; see the doc comment on
// listExchangeableAzureRIs).
//
// The scoped session is authorized for subscription sub-1 (requireAzureSubscriptionScope
// passes), and the tenant-wide listing the injected client returns mixes rows
// billed to sub-1 and sub-2 -- the same tenant-wide breadth
// ListExchangeableReservations always returns regardless of which
// subscription's credentials made the call. Only the sub-1 row must survive.
func TestListExchangeableAzureRIs_SubscriptionIDFiltersToOwnRows(t *testing.T) {
	ctx := context.Background()
	store := &MockConfigStore{}
	store.GetCloudAccountByExternalIDFn = func(_ context.Context, provider, externalID string) (*config.CloudAccount, error) {
		require.Equal(t, "azure", provider)
		require.Equal(t, "sub-1", externalID)
		return &config.CloudAccount{ID: "acct-mine", Name: "Mine Team", Provider: "azure", ExternalID: "sub-1"}, nil
	}
	stub := &stubAzureExchangeClient{reservations: []azurecompute.ExchangeableReservation{
		{ReservationID: "res-own", BillingScopeID: "/subscriptions/sub-1", SKU: "Standard_D2s_v3", Quantity: 1, Region: "eastus"},
		{ReservationID: "res-other", BillingScopeID: "/subscriptions/sub-2", SKU: "Standard_D4s_v3", Quantity: 5, Region: "westeurope"},
	}}

	h := &Handler{
		auth:                 scopedAzureAuth(t, "view", "purchases", []string{"acct-mine"}),
		config:               store,
		azureExchangeFactory: func(_ string) azureExchangeClient { return stub },
	}

	res, err := h.listExchangeableAzureRIs(ctx, &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"authorization": "Bearer tok"},
		QueryStringParameters: map[string]string{"subscription_id": "sub-1"},
	})
	require.NoError(t, err)
	resp, ok := res.(*ExchangeableAzureRIsResponse)
	require.True(t, ok)
	require.Len(t, resp.Reservations, 1, "only the requested subscription's own reservation must be returned")
	assert.Equal(t, "res-own", resp.Reservations[0].ReservationID)
	for _, r := range resp.Reservations {
		assert.NotEqual(t, "res-other", r.ReservationID, "a reservation billed to a different subscription must never appear")
	}
}

// --- Azure credential-resolution path tests (issue LeanerCloud/cloud-commitments-cli#932) ---
//
// These tests exercise the production path of buildAzureExchangeClient, which
// was previously hardcoded to azidentity.NewDefaultAzureCredential. The fix
// routes through the project's per-subscription resolver so Lambda (which has
// no ambient Azure identity) stops 500ing.
//
// No real Azure calls are made: the MockConfigStore.GetCloudAccountByExternalID
// override supplies a fake CloudAccount, and the azureExchangeFactory is NOT
// wired — the test exercises the real buildAzureExchangeClient production path.
// The credential store returns nil from LoadRaw, which causes
// ResolveAzureTokenCredentialWithOpts to fail when the account uses client_secret
// mode (expected: we assert the error rather than a 500). For the graceful-empty
// path (no account registered) we assert a clean empty response.

// TestListExchangeableAzureRIs_NoAzureAccountRegistered verifies that when no
// Azure CloudAccount is registered for the requested subscription, the handler
// returns an empty reservations list (graceful empty state) rather than a 500.
// This is the fix for issue LeanerCloud/cloud-commitments-cli#932: Lambda has no
// ambient Azure identity so the old DefaultAzureCredential call failed,
// producing an opaque 500.
func TestListExchangeableAzureRIs_NoAzureAccountRegistered(t *testing.T) {
	mockStore := &MockConfigStore{}
	// GetCloudAccountByExternalID returns (nil, nil) — no account for this sub.
	mockStore.GetCloudAccountByExternalIDFn = func(_ context.Context, provider, externalID string) (*config.CloudAccount, error) {
		require.Equal(t, "azure", provider, "provider must be azure")
		require.Equal(t, "sub-not-registered", externalID)
		return nil, nil
	}

	h := &Handler{
		auth:   &mockAuthForExchange{},
		config: mockStore,
		// azureExchangeFactory is intentionally NOT set so the production
		// buildAzureExchangeClient path runs (verifying it handles nil account).
	}

	res, err := h.listExchangeableAzureRIs(context.Background(), &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"authorization": "Bearer test-token"},
		QueryStringParameters: map[string]string{"subscription_id": "sub-not-registered"},
	})
	require.NoError(t, err, "missing Azure account must produce empty state, not an error")
	resp, ok := res.(*ExchangeableAzureRIsResponse)
	require.True(t, ok)
	assert.Empty(t, resp.Reservations, "no Azure account registered must return empty reservation list")
}

// TestListExchangeableAzureRIs_EmptySubscriptionNoAccounts verifies the graceful
// empty state when subscription_id is omitted and no Azure accounts are registered.
func TestListExchangeableAzureRIs_EmptySubscriptionNoAccounts(t *testing.T) {
	mockStore := &MockConfigStore{}
	// GetCloudAccountByExternalID with empty externalID also returns (nil, nil).
	mockStore.GetCloudAccountByExternalIDFn = func(_ context.Context, provider, externalID string) (*config.CloudAccount, error) {
		require.Equal(t, "azure", provider)
		return nil, nil
	}

	h := &Handler{
		auth:   &mockAuthForExchange{},
		config: mockStore,
	}

	res, err := h.listExchangeableAzureRIs(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer test-token"},
		// No subscription_id parameter.
	})
	require.NoError(t, err, "no Azure accounts + no subscription_id must return empty state")
	resp, ok := res.(*ExchangeableAzureRIsResponse)
	require.True(t, ok)
	assert.Empty(t, resp.Reservations)
}

// TestListExchangeableAzureRIs_AccountLookupError verifies that a DB error from
// GetCloudAccountByExternalID surfaces as an error (which maps to 500) rather
// than silently falling through. A DB outage is a server-side fault, not a
// client mistake, so 5xx is the correct classification.
func TestListExchangeableAzureRIs_AccountLookupError(t *testing.T) {
	mockStore := &MockConfigStore{}
	mockStore.GetCloudAccountByExternalIDFn = func(_ context.Context, provider, _ string) (*config.CloudAccount, error) {
		return nil, fmt.Errorf("database connection lost")
	}

	h := &Handler{
		auth:   &mockAuthForExchange{},
		config: mockStore,
	}

	_, err := h.listExchangeableAzureRIs(context.Background(), &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"authorization": "Bearer test-token"},
		QueryStringParameters: map[string]string{"subscription_id": "sub-db-error"},
	})
	require.Error(t, err, "DB lookup failure must propagate as an error")
	// Must NOT be a 4xx ClientError: DB failures are server-side faults.
	_, isClientErr := IsClientError(err)
	assert.False(t, isClientErr, "DB lookup failure must not be classified as a client (4xx) error")
	assert.Contains(t, err.Error(), "database connection lost")
}

// TestListExchangeableAzureRIs_CredentialResolutionError verifies that when a
// CloudAccount IS registered but credential resolution fails (e.g. missing stored
// secret), the error propagates as a server-side error. This guards against the
// regression where a misconfigured account silently returned an empty list
// instead of surfacing the configuration problem to the operator.
func TestListExchangeableAzureRIs_CredentialResolutionError(t *testing.T) {
	const subID = "sub-cred-error"
	mockStore := &MockConfigStore{}
	// Account exists but uses client_secret mode with no stored secret.
	mockStore.GetCloudAccountByExternalIDFn = func(_ context.Context, provider, externalID string) (*config.CloudAccount, error) {
		require.Equal(t, "azure", provider)
		require.Equal(t, subID, externalID)
		return &config.CloudAccount{
			ID:                  "acct-uuid-001",
			Provider:            "azure",
			ExternalID:          subID,
			AzureSubscriptionID: subID,
			AzureTenantID:       "tenant-001",
			AzureClientID:       "client-001",
			AzureAuthMode:       "client_secret",
			Enabled:             true,
		}, nil
	}

	// credStore.LoadRaw returns nil (no secret stored) which causes
	// ResolveAzureTokenCredentialWithOpts to return an error.
	h := &Handler{
		auth:      &mockAuthForExchange{},
		config:    mockStore,
		credStore: &MockCredentialStore{}, // LoadRaw always returns (nil, nil)
	}

	_, err := h.listExchangeableAzureRIs(context.Background(), &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"authorization": "Bearer test-token"},
		QueryStringParameters: map[string]string{"subscription_id": subID},
	})
	require.Error(t, err, "missing credential must produce an error, not a silent empty state")
	// The error must be server-side (not a 4xx ClientError).
	_, isClientErr := IsClientError(err)
	assert.False(t, isClientErr, "credential resolution failure must not be classified as a client (4xx) error")
}

// TestBuildAzureExchangeClient_UsesResolver verifies that buildAzureExchangeClient
// calls GetCloudAccountByExternalID with the correct (provider, subscriptionID) pair,
// confirming the production path no longer uses DefaultAzureCredential. The subscription
// ID from the request must flow through to the lookup without being altered.
func TestBuildAzureExchangeClient_UsesResolver(t *testing.T) {
	const subID = "sub-resolver-test"
	var capturedProvider, capturedExtID string

	mockStore := &MockConfigStore{}
	mockStore.GetCloudAccountByExternalIDFn = func(_ context.Context, provider, externalID string) (*config.CloudAccount, error) {
		capturedProvider = provider
		capturedExtID = externalID
		// Return nil to short-circuit credential resolution (graceful empty path).
		return nil, nil
	}

	h := &Handler{
		config: mockStore,
		// No azureExchangeFactory: production path runs.
	}

	client, err := h.buildAzureExchangeClient(context.Background(), subID)
	require.NoError(t, err)
	assert.Nil(t, client, "nil account must yield nil client (graceful empty path)")
	assert.Equal(t, "azure", capturedProvider, "lookup must use provider=azure")
	assert.Equal(t, subID, capturedExtID, "lookup must use the subscription ID from the request")
}

// mockAuthForExchange is a minimal auth mock that returns an admin session.
type mockAuthForExchange struct{}

func (m *mockAuthForExchange) Login(_ context.Context, _ LoginRequest) (*LoginResponse, error) {
	return nil, nil
}
func (m *mockAuthForExchange) Logout(_ context.Context, _ string) error { return nil }
func (m *mockAuthForExchange) ValidateSession(_ context.Context, _ string) (*Session, error) {
	return &Session{UserID: "admin", Email: "admin@test.com"}, nil
}
func (m *mockAuthForExchange) ValidateCSRFToken(_ context.Context, _, _ string) error { return nil }
func (m *mockAuthForExchange) SetupAdmin(_ context.Context, _ SetupAdminRequest) (*LoginResponse, error) {
	return nil, nil
}
func (m *mockAuthForExchange) CheckAdminExists(_ context.Context) (bool, error)       { return true, nil }
func (m *mockAuthForExchange) RequestPasswordReset(_ context.Context, _ string) error { return nil }
func (m *mockAuthForExchange) ConfirmPasswordReset(_ context.Context, _ PasswordResetConfirm) error {
	return nil
}
func (m *mockAuthForExchange) ResetTokenStatus(_ context.Context, _ string) (string, string, error) {
	return "valid", "reset", nil
}
func (m *mockAuthForExchange) GetUser(_ context.Context, _ string) (*User, error) { return nil, nil }
func (m *mockAuthForExchange) UpdateUserProfile(_ context.Context, _, _, _, _ string) error {
	return nil
}
func (m *mockAuthForExchange) CreateUserAPI(_ context.Context, _ string, _ auth.APICreateUserRequest) (*auth.APICreateUserResponse, error) {
	return nil, nil
}
func (m *mockAuthForExchange) UpdateUserAPI(_ context.Context, _, _ string, _ auth.APIUpdateUserRequest) (*auth.APIUser, error) {
	return nil, nil
}
func (m *mockAuthForExchange) DeleteUser(_ context.Context, _ string) error              { return nil }
func (m *mockAuthForExchange) ListUsersAPI(_ context.Context) (any, error)               { return nil, nil }
func (m *mockAuthForExchange) ChangePasswordAPI(_ context.Context, _, _, _ string) error { return nil }
func (m *mockAuthForExchange) CreateGroupAPI(_ context.Context, _ string, _ auth.APICreateGroupRequest) (*auth.APIGroup, error) {
	return nil, nil
}
func (m *mockAuthForExchange) UpdateGroupAPI(_ context.Context, _, _ string, _ auth.APIUpdateGroupRequest) (*auth.APIGroup, error) {
	return nil, nil
}
func (m *mockAuthForExchange) DeleteGroup(_ context.Context, _ string) error        { return nil }
func (m *mockAuthForExchange) GetGroupAPI(_ context.Context, _ string) (any, error) { return nil, nil }
func (m *mockAuthForExchange) ListGroupsAPI(_ context.Context) (any, error)         { return nil, nil }
func (m *mockAuthForExchange) HasPermissionAPI(_ context.Context, _, _, _ string) (bool, error) {
	return true, nil
}
func (m *mockAuthForExchange) HasPermissionForConstraintsAPI(_ context.Context, _, _, _ string, _ []auth.PermissionConstraints) (bool, error) {
	return true, nil
}
func (m *mockAuthForExchange) GetUserPermissionsAPI(_ context.Context, _ string) (any, error) {
	return nil, nil
}
func (m *mockAuthForExchange) CreateAPIKeyAPI(_ context.Context, _ string, _ auth.APICreateAPIKeyRequest) (any, error) {
	return nil, nil
}
func (m *mockAuthForExchange) ListUserAPIKeysAPI(_ context.Context, _ string) (any, error) {
	return nil, nil
}
func (m *mockAuthForExchange) GetAPIKeysUsageStatsAPI(_ context.Context, _ string) (any, error) {
	return nil, nil
}
func (m *mockAuthForExchange) DeleteAPIKeyAPI(_ context.Context, _, _ string) error { return nil }
func (m *mockAuthForExchange) RevokeAPIKeyAPI(_ context.Context, _, _ string) error { return nil }
func (m *mockAuthForExchange) ValidateUserAPIKeyAPI(_ context.Context, _ string) (any, any, error) {
	return nil, nil, nil
}
func (m *mockAuthForExchange) RecordAPIKeyUsageAsync(_ string) {}
func (m *mockAuthForExchange) GetAPIKeyEffectivePermissionsAPI(_ context.Context, _, _ string) ([]auth.APIPermission, error) {
	return nil, nil
}
func (m *mockAuthForExchange) HasAPIKeyPermissionAPI(_ context.Context, _, _, _ string) (string, string, bool, error) {
	return "admin", "", true, nil
}
func (m *mockAuthForExchange) HasAPIKeyPermissionForConstraintsAPI(_ context.Context, _, _, _, _ string, _ []auth.PermissionConstraints) (bool, error) {
	return true, nil
}
func (m *mockAuthForExchange) GetAllowedAccountsAPI(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (m *mockAuthForExchange) MFASetupAPI(_ context.Context, _, _ string) (string, string, error) {
	return "", "", nil
}
func (m *mockAuthForExchange) MFAEnableAPI(_ context.Context, _, _ string) ([]string, error) {
	return nil, nil
}
func (m *mockAuthForExchange) MFADisableAPI(_ context.Context, _, _, _ string) error { return nil }
func (m *mockAuthForExchange) MFARegenerateRecoveryCodesAPI(_ context.Context, _, _ string) ([]string, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// Defect 1 backend: GET /api/ri-exchange/target-offerings
// ---------------------------------------------------------------------------

// stubTargetOfferingsEC2 is a test stub for targetOfferingsEC2Client.
// It returns a fixed list of ConvertibleRIs for the lookup step and a
// fixed list of TargetOfferings for the DescribeReservedInstancesOfferings
// step. Both are configurable per-test so we can exercise the 404 and
// happy paths without live AWS.
type stubTargetOfferingsEC2 struct {
	err       error
	instances []ec2svc.ConvertibleRI
	offerings []ec2svc.TargetOffering
}

func (s *stubTargetOfferingsEC2) ListConvertibleReservedInstances(_ context.Context) ([]ec2svc.ConvertibleRI, error) {
	return s.instances, s.err
}

func (s *stubTargetOfferingsEC2) ListTargetOfferings(_ context.Context, _ ec2svc.ListTargetOfferingsParams) ([]ec2svc.TargetOffering, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.offerings, nil
}

func TestListTargetOfferings_RequiresPermission(t *testing.T) {
	h := &Handler{}
	_, err := h.listTargetOfferings(context.Background(), &events.LambdaFunctionURLRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "authentication")
}

func TestListTargetOfferings_MissingSourceRIID(t *testing.T) {
	h := &Handler{auth: &mockAuthForExchange{}}
	_, err := h.listTargetOfferings(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer test-token"},
	})
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, 400, ce.code)
	assert.Contains(t, err.Error(), "source_ri_id is required")
}

func TestListTargetOfferings_SourceRINotFound(t *testing.T) {
	stub := &stubTargetOfferingsEC2{
		instances: []ec2svc.ConvertibleRI{
			{ReservedInstanceID: "ri-known", InstanceType: "m5.large"},
		},
	}
	h := &Handler{
		auth:                      &mockAuthForExchange{},
		targetOfferingsEC2Factory: func(_ aws.Config) targetOfferingsEC2Client { return stub },
	}
	h.awsCfgOnce.Do(func() { h.awsCfg = aws.Config{Region: "us-east-1"} })

	_, err := h.listTargetOfferings(context.Background(), &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"authorization": "Bearer test-token"},
		QueryStringParameters: map[string]string{"source_ri_id": "ri-unknown"},
	})
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, 404, ce.code)
	assert.Contains(t, err.Error(), "ri-unknown")
}

func TestListTargetOfferings_HappyPath(t *testing.T) {
	sourceID := "296818b6-73f8-4cd2-94bc-dbb95f794812"
	stub := &stubTargetOfferingsEC2{
		instances: []ec2svc.ConvertibleRI{
			{
				ReservedInstanceID: sourceID,
				InstanceType:       "t3.medium",
				ProductDescription: "Linux/UNIX",
				InstanceTenancy:    "default",
				Scope:              "Region",
				Duration:           31536000,
				OfferingType:       "No Upfront",
			},
		},
		offerings: []ec2svc.TargetOffering{
			{OfferingID: "4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91", InstanceType: "m5.large", OfferingType: "No Upfront"},
			{OfferingID: "7e1234aa-0000-4567-abcd-ef0123456789", InstanceType: "m6i.large", OfferingType: "No Upfront"},
		},
	}
	h := &Handler{
		auth:                      &mockAuthForExchange{},
		targetOfferingsEC2Factory: func(_ aws.Config) targetOfferingsEC2Client { return stub },
	}
	h.awsCfgOnce.Do(func() { h.awsCfg = aws.Config{Region: "us-east-1"} })

	res, err := h.listTargetOfferings(context.Background(), &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"authorization": "Bearer test-token"},
		QueryStringParameters: map[string]string{"source_ri_id": sourceID},
	})
	require.NoError(t, err)
	resp, ok := res.(*TargetOfferingsResponse)
	require.True(t, ok)
	require.Len(t, resp.Offerings, 2)
	assert.Equal(t, "4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91", resp.Offerings[0].OfferingID)
	assert.Equal(t, "m5.large", resp.Offerings[0].InstanceType)
}

// ---------------------------------------------------------------------------
// Defect 2: offering-id UUID format validation
// ---------------------------------------------------------------------------

func TestGetExchangeQuote_InvalidOfferingIDFormat(t *testing.T) {
	h := &Handler{auth: &mockAuthForExchange{}}

	// "t3.medium" looks like an instance type, not an offering UUID
	_, err := h.getExchangeQuote(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer test-token"},
		Body:    `{"ri_ids":["ri-123"],"targets":[{"offering_id":"t3.medium","count":1}]}`,
	})
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, 400, ce.code)
	assert.Contains(t, err.Error(), "t3.medium")
	assert.Contains(t, err.Error(), "offering UUID")
}

func TestGetExchangeQuote_ValidOfferingIDPassesFormat(t *testing.T) {
	// A well-formed UUID should pass the format check and reach the quote
	// call. The quote function is stubbed so the test never resolves AWS
	// credentials or calls EC2 (#855).
	var quotes []exchange.ExchangeQuoteRequest
	h := &Handler{
		auth: &mockAuthForExchange{},
		exchangeQuoteFn: func(_ context.Context, req exchange.ExchangeQuoteRequest) (*exchange.ExchangeQuoteSummary, error) {
			quotes = append(quotes, req)
			return &exchange.ExchangeQuoteSummary{IsValidExchange: true}, nil
		},
	}
	h.awsCfgOnce.Do(func() { h.awsCfg = aws.Config{Region: "us-east-1"} })

	_, err := h.getExchangeQuote(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer test-token"},
		Body:    `{"ri_ids":["ri-123"],"targets":[{"offering_id":"4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91","count":1}]}`,
	})
	require.NoError(t, err, "a valid UUID must not be rejected by the format check")
	require.Len(t, quotes, 1, "a valid UUID must reach the quote call")
	assert.Equal(t, "4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91", quotes[0].Targets[0].OfferingID)
}

func TestValidateExecuteExchangeBody_InvalidOfferingIDFormat(t *testing.T) {
	err := validateExecuteExchangeBody(ExchangeExecuteRequestBody{
		RIIDs:            []string{"ri-123"},
		Targets:          []ExchangeTargetBody{{OfferingID: "t3.medium", Count: 1}},
		MaxPaymentDueUSD: "50.00",
	})
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, 400, ce.code)
	assert.Contains(t, err.Error(), "t3.medium")
	assert.Contains(t, err.Error(), "offering UUID")
}

// ---------------------------------------------------------------------------
// Defect 3: AWS 4xx -> client 4xx, error message preserved
// ---------------------------------------------------------------------------

// fakeAPIError is a minimal smithy.APIError implementation for tests.
type fakeAPIError struct {
	code    string
	message string
}

func (e *fakeAPIError) Error() string                 { return fmt.Sprintf("%s: %s", e.code, e.message) }
func (e *fakeAPIError) ErrorCode() string             { return e.code }
func (e *fakeAPIError) ErrorMessage() string          { return e.message }
func (e *fakeAPIError) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

func TestMapAWSExchangeError_ClientFault4xx(t *testing.T) {
	codes := []string{
		"InvalidOfferingId",
		"InvalidParameter",
		"ValidationError",
		"InvalidReservedInstancesId.NotFound",
		"InvalidInstanceID.NotFound",
	}
	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			apiErr := &fakeAPIError{code: code, message: "AWS says: bad input"}
			mapped := mapAWSExchangeError("fallback msg", apiErr)
			ce, ok := IsClientError(mapped)
			require.True(t, ok, "must be ClientError")
			assert.Equal(t, 400, ce.code, "client-fault codes must map to 400")
			assert.Contains(t, mapped.Error(), "AWS says: bad input",
				"AWS error message must be preserved")
		})
	}
}

func TestMapAWSExchangeError_ServerFault5xx(t *testing.T) {
	// An AWS error with an unrecognized code must stay 500
	apiErr := &fakeAPIError{code: "InternalError", message: "AWS is having a bad day"}
	mapped := mapAWSExchangeError("exchange quote failed", apiErr)
	ce, ok := IsClientError(mapped)
	require.True(t, ok)
	assert.Equal(t, 500, ce.code)
	assert.Contains(t, mapped.Error(), "exchange quote failed")
	assert.NotContains(t, mapped.Error(), "AWS is having a bad day",
		"non-client-fault AWS message must NOT leak through")
}

func TestMapAWSExchangeError_NonAWSError(t *testing.T) {
	// A plain Go error must also stay 500
	mapped := mapAWSExchangeError("exchange quote failed", fmt.Errorf("network timeout"))
	ce, ok := IsClientError(mapped)
	require.True(t, ok)
	assert.Equal(t, 500, ce.code)
}

// TestExecuteExchange_EmptyRegionReturns400 pins finding 01-L4:
// executeExchange must reject a request with no region rather than
// silently routing the exchange to us-east-1, where RIs may not live.
// A wrong-region execute is a financially irreversible mistake.
func TestExecuteExchange_EmptyRegionReturns400(t *testing.T) {
	h := &Handler{auth: &mockAuthForExchange{}}

	_, err := h.executeExchange(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer test-token"},
		Body:    `{"ri_ids":["ri-123"],"target_offering_id":"off-1","target_count":1,"max_payment_due_usd":"10.00"}`,
	})
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a ClientError, got: %v", err)
	assert.Equal(t, 400, ce.code)
	assert.Contains(t, err.Error(), "region is required")
}

// TestExecuteExchange_PermissionConstraintsDenied is the SEC-01 (#1141)
// regression test for the exchange path: a session that passes the bare
// execute:ri-exchange gate (as a constrained permission does) but whose
// per-permission Constraints reject the request must receive a 403 before
// the exchange is submitted to AWS. The constraint set must carry the AWS
// EC2 scope, the request's region, the deployment's registered cloud
// account, and the max_payment_due_usd guardrail as the amount.
func TestExecuteExchange_PermissionConstraintsDenied(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	const deploymentAccountID = "11111111-2222-3333-4444-555555555555"
	userSession := &Session{
		UserID: "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee",
		Email:  "exchanger@example.com",
	}
	mockAuth.On("ValidateSession", ctx, "exchange-token").Return(userSession, nil)
	mockAuth.On("HasPermissionAPI", ctx, userSession.UserID, "execute", "ri-exchange").Return(true, nil)
	// The session-scope gate (reshapeCloudAccountInScope, issue #93) runs
	// before the permission-constraints check this test targets; unrestrict
	// it so the request reaches the constraints check under test.
	allowAnyAccountScope(mockAuth)
	mockAuth.On("HasPermissionForConstraintsAPI", ctx, userSession.UserID, "execute", "ri-exchange",
		mock.MatchedBy(func(sets []auth.PermissionConstraints) bool {
			if len(sets) != 1 {
				return false
			}
			c := sets[0]
			return assert.ObjectsAreEqual([]string{deploymentAccountID}, c.AccountIDs) &&
				assert.ObjectsAreEqual([]string{"aws"}, c.Providers) &&
				assert.ObjectsAreEqual([]string{"ec2"}, c.Services) &&
				assert.ObjectsAreEqual([]string{"eu-central-1"}, c.Regions) &&
				c.MaxPurchaseAmount == 250.50
		})).Return(false, nil)

	h := &Handler{
		auth: mockAuth,
		reshapeAccountResolver: func(_ context.Context) (string, error) {
			return deploymentAccountID, nil
		},
	}
	_, err := h.executeExchange(ctx, &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer exchange-token"},
		Body:    `{"ri_ids":["ri-123"],"target_offering_id":"off-1","target_count":1,"max_payment_due_usd":"250.50","region":"eu-central-1"}`,
	})
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a ClientError, got: %v", err)
	assert.Equal(t, 403, ce.code)
	assert.Contains(t, ce.Error(), "constraints")
}

// TestExecuteExchange_AccountResolutionErrorFailsClosed pins the fail-closed
// behavior of the SEC-01 AccountIDs dimension: when the deployment's cloud
// account cannot be resolved (STS error, account lookup failure), the
// handler must abort BEFORE the constraint check and the AWS call rather
// than evaluating constraints without the account dimension.
func TestExecuteExchange_AccountResolutionErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	// No HasPermissionForConstraintsAPI expectation: it must NOT be called.
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	userSession := &Session{
		UserID: "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee",
		Email:  "exchanger@example.com",
	}
	mockAuth.On("ValidateSession", ctx, "exchange-token").Return(userSession, nil)
	mockAuth.On("HasPermissionAPI", ctx, userSession.UserID, "execute", "ri-exchange").Return(true, nil)
	// Unrestricted session scope so the reshapeCloudAccountInScope gate
	// (issue #93) short-circuits without needing the resolver, and this
	// test's own resolver failure is the one that fails closed below.
	allowAnyAccountScope(mockAuth)

	h := &Handler{
		auth: mockAuth,
		reshapeAccountResolver: func(_ context.Context) (string, error) {
			return "", fmt.Errorf("sts get-caller-identity denied")
		},
	}
	_, err := h.executeExchange(ctx, &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer exchange-token"},
		Body:    `{"ri_ids":["ri-123"],"target_offering_id":"off-1","target_count":1,"max_payment_due_usd":"250.50","region":"eu-central-1"}`,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolve cloud account scope")
}

// TestExecuteExchange_UnattributedAccountStillConstrained pins the sentinel
// behavior: when the deployment resolves to no registered cloud account
// (non-AWS host, bootstrap), the constraint set must still carry a non-empty
// AccountIDs list (the unattributed sentinel) so a permission constrained to
// specific accounts denies the exchange instead of matching the auth
// layer's "empty request list = satisfied" rule (fail closed).
func TestExecuteExchange_UnattributedAccountStillConstrained(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	userSession := &Session{
		UserID: "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee",
		Email:  "exchanger@example.com",
	}
	mockAuth.On("ValidateSession", ctx, "exchange-token").Return(userSession, nil)
	mockAuth.On("HasPermissionAPI", ctx, userSession.UserID, "execute", "ri-exchange").Return(true, nil)
	// Unrestricted session scope so the reshapeCloudAccountInScope gate
	// (issue #93) is satisfied without consulting the resolver, leaving this
	// test's own single resolver call (for the constraint set below) intact.
	allowAnyAccountScope(mockAuth)
	mockAuth.On("HasPermissionForConstraintsAPI", ctx, userSession.UserID, "execute", "ri-exchange",
		mock.MatchedBy(func(sets []auth.PermissionConstraints) bool {
			return len(sets) == 1 &&
				assert.ObjectsAreEqual([]string{unattributedAccountConstraint}, sets[0].AccountIDs)
		})).Return(false, nil)

	h := &Handler{
		auth: mockAuth,
		reshapeAccountResolver: func(_ context.Context) (string, error) {
			return "", nil
		},
	}
	_, err := h.executeExchange(ctx, &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer exchange-token"},
		Body:    `{"ri_ids":["ri-123"],"target_offering_id":"off-1","target_count":1,"max_payment_due_usd":"250.50","region":"eu-central-1"}`,
	})
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a ClientError, got: %v", err)
	assert.Equal(t, 403, ce.code)
}

// TestExecuteExchange_SessionAccountScopeMismatchReturns403 is the regression
// test for issue #93: listConvertibleRIs, getRIUtilization and
// getReshapeRecommendations each call reshapeCloudAccountInScope and return an
// empty list when the deployment's registered AWS cloud account falls outside
// the session's allowed_accounts, but executeExchange did not, even though it
// is the only one of the four RI-exchange endpoints that is financially
// irreversible.
//
// This replays the issue's exact scenario: a session scoped to
// allowed_accounts=[acct-A] holding an execute:ri-exchange grant with NO
// AccountIDs constraint (so the SEC-01/#1141 requirePermissionConstraints
// check below would allow it unconditionally) attempts to execute against a
// deployment whose ambient AWS account resolves to acct-B. Before the fix
// this reached requirePermissionConstraints and beyond; it must instead be
// refused with 403 by the session-scope gate before either the constraints
// check or the AWS call.
func TestExecuteExchange_SessionAccountScopeMismatchReturns403(t *testing.T) {
	ctx := context.Background()
	const allowedAccountID = "11111111-2222-3333-4444-555555555555"    // acct-A: in the session's scope
	const deploymentAccountID = "99999999-8888-7777-6666-555555555555" // acct-B: the deployment's ambient account

	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	userSession := &Session{
		UserID: "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee",
		Email:  "exchanger@example.com",
	}
	mockAuth.On("ValidateSession", ctx, "exchange-token").Return(userSession, nil)
	mockAuth.On("HasPermissionAPI", ctx, userSession.UserID, "execute", "ri-exchange").Return(true, nil)
	// The session is restricted to acct-A only.
	mockAuth.On("GetAllowedAccountsAPI", ctx, userSession.UserID).Return([]string{allowedAccountID}, nil)
	// No HasPermissionForConstraintsAPI expectation: the permission carries no
	// AccountIDs constraint (matching the issue's failure scenario) and would
	// satisfy that check unconditionally, so it must never be reached -- the
	// session-scope gate above must refuse the request first. Left
	// deliberately unstubbed rather than stubbed-and-asserted-not-called: on
	// the pre-fix handler this call happens and testify panics on the
	// unstubbed mock, which fails the test just as surely (and for the same
	// reason) as an assertion would; the AssertNotCalled below still proves
	// the post-fix ordering.

	mockStore := new(MockConfigStore)
	mockStore.ListCloudAccountsFn = func(_ context.Context, _ config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return nil, nil
	}

	h := &Handler{
		auth:   mockAuth,
		config: mockStore,
		reshapeAccountResolver: func(_ context.Context) (string, error) {
			return deploymentAccountID, nil
		},
	}
	_, err := h.executeExchange(ctx, &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer exchange-token"},
		Body:    `{"ri_ids":["ri-123"],"target_offering_id":"off-1","target_count":1,"max_payment_due_usd":"250.50","region":"eu-central-1"}`,
	})
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a ClientError, got: %v", err)
	assert.Equal(t, 403, ce.code)
	assert.Contains(t, ce.Error(), "allowed accounts")
	mockAuth.AssertNotCalled(t, "HasPermissionForConstraintsAPI",
		mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestExecuteExchange_SessionAccountScopeMatchReachesConstraintsCheck is the
// positive-path counterpart to TestExecuteExchange_SessionAccountScopeMismatchReturns403:
// a session restricted to allowed_accounts that DOES include the deployment's
// cloud account must clear the new session-scope gate and reach the existing
// SEC-01/#1141 permission-constraints check, proving the fix does not
// over-deny an in-scope request. HasPermissionForConstraintsAPI is stubbed to
// deny rather than mocking the downstream AWS call; the "constraints" (not
// "allowed accounts") substring in the resulting error distinguishes which
// gate produced it.
func TestExecuteExchange_SessionAccountScopeMatchReachesConstraintsCheck(t *testing.T) {
	ctx := context.Background()
	const deploymentAccountID = "11111111-2222-3333-4444-555555555555"

	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	userSession := &Session{
		UserID: "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee",
		Email:  "exchanger@example.com",
	}
	mockAuth.On("ValidateSession", ctx, "exchange-token").Return(userSession, nil)
	mockAuth.On("HasPermissionAPI", ctx, userSession.UserID, "execute", "ri-exchange").Return(true, nil)
	// The session is restricted, but its allowed list includes the
	// deployment's own account, so the scope gate must let this through.
	mockAuth.On("GetAllowedAccountsAPI", ctx, userSession.UserID).Return([]string{deploymentAccountID}, nil)
	mockAuth.On("HasPermissionForConstraintsAPI", ctx, userSession.UserID, "execute", "ri-exchange", mock.Anything).
		Return(false, nil)

	mockStore := new(MockConfigStore)
	mockStore.ListCloudAccountsFn = func(_ context.Context, _ config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return nil, nil
	}

	h := &Handler{
		auth:   mockAuth,
		config: mockStore,
		reshapeAccountResolver: func(_ context.Context) (string, error) {
			return deploymentAccountID, nil
		},
	}
	_, err := h.executeExchange(ctx, &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer exchange-token"},
		Body:    `{"ri_ids":["ri-123"],"target_offering_id":"off-1","target_count":1,"max_payment_due_usd":"250.50","region":"eu-central-1"}`,
	})
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a ClientError, got: %v", err)
	assert.Equal(t, 403, ce.code)
	assert.Contains(t, ce.Error(), "constraints", "must be refused by the constraints check, not the session-scope gate")
}

// TestExecuteExchange_SessionScopeLookupErrorFailsClosed pins the fail-closed
// behavior of the new session-scope gate itself: a failure resolving the
// session's allowed_accounts (auth store outage, etc.) must propagate as an
// error, not collapse into a 403 that would misreport a lookup failure as an
// out-of-scope denial, and must abort before the constraints check runs.
func TestExecuteExchange_SessionScopeLookupErrorFailsClosed(t *testing.T) {
	ctx := context.Background()

	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	userSession := &Session{
		UserID: "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee",
		Email:  "exchanger@example.com",
	}
	mockAuth.On("ValidateSession", ctx, "exchange-token").Return(userSession, nil)
	mockAuth.On("HasPermissionAPI", ctx, userSession.UserID, "execute", "ri-exchange").Return(true, nil)
	mockAuth.On("GetAllowedAccountsAPI", ctx, userSession.UserID).
		Return(nil, fmt.Errorf("auth store unavailable"))
	// No HasPermissionForConstraintsAPI expectation: the scope-lookup failure
	// must abort before the constraints check is ever reached.

	h := &Handler{auth: mockAuth}
	_, err := h.executeExchange(ctx, &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer exchange-token"},
		Body:    `{"ri_ids":["ri-123"],"target_offering_id":"off-1","target_count":1,"max_payment_due_usd":"250.50","region":"eu-central-1"}`,
	})
	require.Error(t, err)
	_, ok := IsClientError(err)
	assert.False(t, ok, "a scope-lookup failure must not be reported as a ClientError, got: %v", err)
	assert.Contains(t, err.Error(), "failed to get allowed accounts")
	mockAuth.AssertNotCalled(t, "HasPermissionForConstraintsAPI",
		mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestGetExchangeQuote_EmptyRegionResolvesFromSDK pins finding 01-L4:
// getExchangeQuote must resolve the region from the AWS SDK chain when
// the caller omits it, matching getReshapeRecommendations, instead of
// hardcoding us-east-1. We pre-seal awsCfgOnce with a non-default region
// and verify the (stubbed, #855) quote call receives that region and the
// response echoes it.
func TestGetExchangeQuote_EmptyRegionResolvesFromSDK(t *testing.T) {
	const cfgRegion = "eu-west-1"
	var quotes []exchange.ExchangeQuoteRequest
	h := &Handler{
		auth: &mockAuthForExchange{},
		exchangeQuoteFn: func(_ context.Context, req exchange.ExchangeQuoteRequest) (*exchange.ExchangeQuoteSummary, error) {
			quotes = append(quotes, req)
			return &exchange.ExchangeQuoteSummary{IsValidExchange: true}, nil
		},
	}
	// Pre-seal awsCfgOnce so loadAWSConfigWithRegion returns cfgRegion
	// without making a real SDK call.
	h.awsCfgOnce.Do(func() {
		h.awsCfg = aws.Config{Region: cfgRegion}
	})

	resp, err := h.getExchangeQuote(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer test-token"},
		Body:    `{"ri_ids":["ri-123"],"target_offering_id":"off-1","target_count":1}`,
	})
	require.NoError(t, err, "getExchangeQuote must not reject a missing region; it must resolve from the SDK chain")
	require.Len(t, quotes, 1)
	assert.Equal(t, cfgRegion, quotes[0].Region, "the quote must use the SDK-resolved region")
	typed, ok := resp.(ExchangeQuoteResponse)
	require.True(t, ok, "expected ExchangeQuoteResponse, got %T", resp)
	assert.Equal(t, cfgRegion, typed.Region, "the response must echo the SDK-resolved region")
}

// TestExchangeQuoteResponse_MarshalsRegionAlongsideSummaryFields pins
// issue #238: the frontend modal learns the region the backend resolved
// only from the quote response, then must echo it back unchanged on
// execute (which rejects a missing region outright). Pre-fix,
// getExchangeQuote returned the bare *exchange.ExchangeQuoteSummary with
// no region field at all, so there was no way for the frontend to send
// one on execute. This asserts the wrapper both carries "Region" in the
// marshaled JSON and still promotes the embedded summary's own fields
// to the top level, so this change is additive from the frontend's
// point of view.
func TestExchangeQuoteResponse_MarshalsRegionAlongsideSummaryFields(t *testing.T) {
	resp := ExchangeQuoteResponse{
		ExchangeQuoteSummary: &exchange.ExchangeQuoteSummary{
			IsValidExchange: true,
			PaymentDueRaw:   "12.50",
		},
		Region: "eu-west-1",
	}

	raw, err := json.Marshal(resp)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))

	assert.Equal(t, "eu-west-1", decoded["Region"],
		"the region the SDK chain resolved must be present in the quote response JSON")
	assert.Equal(t, true, decoded["IsValidExchange"],
		"the embedded summary's own fields must still be promoted to the top level")
	assert.Equal(t, "12.50", decoded["PaymentDueRaw"])
}

// TestExecuteApprovedExchange_EmptyRecordRegionFails pins finding 01-L4:
// executeApprovedExchange must fail the exchange (not default to us-east-1)
// when the stored record has no region. A hardcoded fallback would execute
// a financially irreversible operation in the wrong region.
func TestExecuteApprovedExchange_EmptyRecordRegionFails(t *testing.T) {
	ctx := context.Background()
	id := "550e8400-e29b-41d4-a716-000000000001"

	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	h := &Handler{config: mockStore}

	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{
		RIExchangeMaxDailyUSD:       1000,
		RIExchangeMaxPerExchangeUSD: 500,
	}, nil)
	// Record has no region — the handler must call failExchange, not proceed.
	mockStore.On("FailRIExchange", mock.Anything, id, mock.MatchedBy(func(reason string) bool {
		return len(reason) > 0
	})).Return(nil)

	record := &config.RIExchangeRecord{
		ID:               id,
		Region:           "", // intentionally empty
		SourceRIIDs:      []string{"ri-1"},
		TargetOfferingID: "offering-1",
		TargetCount:      1,
		PaymentDue:       "50.00",
	}

	resp, err := h.executeApprovedExchange(ctx, id, record)
	require.NoError(t, err, "executeApprovedExchange surfaces the failure via failExchange, not via error return")
	respMap, ok := resp.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "failed", respMap["status"])
	assert.Contains(t, respMap["reason"], "no region")
	mockStore.AssertNotCalled(t, "ReserveApprovedRIExchange", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestClassifyRecsAge pins the staleness classification thresholds for
// the reshape freshness banner. The three transitions are:
//
//   - < 12 h  → "" (fresh, no banner)
//   - 12–24 h → "soft" (warning banner)
//   - >= 24 h → "hard" (critical banner)
func TestClassifyRecsAge(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		age  time.Duration
		want string
	}{
		{"zero age is fresh", 0, ""},
		{"30 minutes is fresh", 30 * time.Minute, ""},
		{"just under soft threshold", reshapeSoftStaleThreshold - time.Minute, ""},
		{"exactly soft threshold", reshapeSoftStaleThreshold, "soft"},
		{"13 hours is soft", 13 * time.Hour, "soft"},
		{"just under hard threshold", reshapeHardStaleThreshold - time.Minute, "soft"},
		{"exactly hard threshold", reshapeHardStaleThreshold, "hard"},
		{"25 hours is hard", 25 * time.Hour, "hard"},
		{"48 hours is hard", 48 * time.Hour, "hard"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyRecsAge(tc.age)
			if got != tc.want {
				t.Errorf("classifyRecsAge(%v) = %q, want %q", tc.age, got, tc.want)
			}
		})
	}
}

// --- Audit actor stamping tests (issue #1009) ---

// TestApproveRIExchange_SessionActorStamped asserts that the session-authed
// approval path passes the session UserID as the actor to TransitionRIExchangeStatus
// so that transitioned_by is set on the ri_exchange_history row.
func TestApproveRIExchange_SessionActorStamped(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	t.Cleanup(func() { mockStore.AssertExpectations(t) })

	const actorID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const id = "550e8400-e29b-41d4-a716-446655441001"

	adminSession := &Session{UserID: actorID, Email: "admin@example.com"}
	mockAuth.On("ValidateSession", ctx, "admin-bearer").Return(adminSession, nil)
	mockAuth.grantAdminPurchaser()
	// Issue #1757: CSRF must be validated before the record fetch.
	mockAuth.On("ValidateCSRFToken", ctx, "admin-bearer", "csrf-abc").Return(nil)

	mockStore.On("GetRIExchangeRecord", ctx, id).Return(&config.RIExchangeRecord{
		ID: id, Status: "pending", ApprovalToken: "tok", SourceRIIDs: []string{"ri-1"}, PaymentDue: "10.00",
	}, nil)
	// Actor must be the session UserID.
	mockStore.On("TransitionRIExchangeStatus", ctx, id, "pending", "processing",
		mock.MatchedBy(func(a *string) bool { return a != nil && *a == actorID }),
	).Return(&config.RIExchangeRecord{ID: id, Status: "processing", SourceRIIDs: []string{"ri-1"}, PaymentDue: "10.00"}, nil)
	mockStore.On("ReserveApprovedRIExchange", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return("500.000000", nil).Maybe()
	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{
		RIExchangeMaxDailyUSD: 1000, RIExchangeMaxPerExchangeUSD: 500,
	}, nil)
	mockStore.On("FailRIExchange", mock.Anything, id, mock.AnythingOfType("string")).Return(nil)
	mockStore.On("StampRIExchangeApprovedBy", mock.Anything, id, adminSession.Email).Return(nil)

	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{
			"authorization": "Bearer admin-bearer",
			"x-csrf-token":  "csrf-abc",
		},
	}
	_, err := (&Handler{config: mockStore, auth: mockAuth}).approveRIExchange(ctx, req, id, "")
	require.NoError(t, err)
}

// TestRejectRIExchange_TokenPathActorIsNil asserts that the token-based rejection
// path passes nil as the actor param so that transitioned_by = NULL on the row.
// Token-based paths have no session, so no human actor can be attributed.
func TestRejectRIExchange_TokenPathActorIsNil(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })

	const id = "550e8400-e29b-41d4-a716-446655441002"

	mockStore.On("GetRIExchangeRecord", ctx, id).Return(&config.RIExchangeRecord{
		ID: id, Status: "pending", ApprovalToken: config.HashApprovalToken("tok"),
	}, nil)
	// Token path: actor must be nil. Canonical spelling used (#1277 follow-up).
	mockStore.On("TransitionRIExchangeStatus", ctx, id, "pending", config.StatusCanceled,
		(*string)(nil),
	).Return(&config.RIExchangeRecord{ID: id, Status: config.StatusCanceled}, nil)

	_, err := (&Handler{config: mockStore}).rejectRIExchange(ctx, id, "tok")
	require.NoError(t, err)
}

// ─── H2: effectiveCap bounded by daily headroom ───────────────────────────────

// TestExecuteApprovedExchange_EffectiveCap_BoundedByDailyHeadroom is the
// regression test for H2: when dailyCap-dailySpent < perExchangeCap, the cap
// passed to Execute (MaxPaymentDueUSD) must be the daily headroom, not the full
// perExchangeCap. This prevents a fresh re-quote from accepting an amount that
// exceeds the daily budget.
func TestExecuteApprovedExchange_EffectiveCap_BoundedByDailyHeadroom(t *testing.T) {
	ctx := context.Background()
	const id = "550e8400-e29b-41d4-a716-000000000012"

	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })

	// dailySpent=$450, dailyCap=$500, perExchangeCap=$100 -> the store reserves
	// the $50 headroom as the ceiling.
	mockStore.On("ReserveApprovedRIExchange", ctx, id, "500.000000", "100.000000").Return("50.000000", nil)
	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{
		RIExchangeMaxDailyUSD:       500,
		RIExchangeMaxPerExchangeUSD: 100,
	}, nil)

	var capturedReq exchange.ExchangeExecuteRequest
	h := &Handler{
		config: mockStore,
		executeExchangeFn: func(_ context.Context, req exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
			capturedReq = req
			return "exch-h2-handler", &exchange.ExchangeQuoteSummary{PaymentDueUSDStr: "40.000000"}, nil
		},
	}

	mockStore.On("CompleteRIExchangeWithPayment", mock.Anything, id, "exch-h2-handler", "40.000000").Return(nil)

	record := &config.RIExchangeRecord{
		ID:               id,
		Region:           "us-east-1",
		SourceRIIDs:      []string{"ri-1"},
		TargetOfferingID: "offering-1",
		TargetCount:      1,
		PaymentDue:       "40.00",
	}

	_, err := h.executeApprovedExchange(ctx, id, record)
	require.NoError(t, err)

	// MaxPaymentDueUSD must equal headroom ($50), not perExchangeCap ($100).
	require.NotNil(t, capturedReq.MaxPaymentDueUSD,
		"MaxPaymentDueUSD must be set on the Execute request")
	expectedCap := new(big.Rat).SetFrac64(50, 1)
	assert.Equal(t, 0, capturedReq.MaxPaymentDueUSD.Cmp(expectedCap),
		"MaxPaymentDueUSD must be daily headroom ($50), got $%s",
		capturedReq.MaxPaymentDueUSD.FloatString(2))
}

// ─── H3: ledger records fresh accepted amount ─────────────────────────────────

// TestExecuteApprovedExchange_AcceptedAmountFromFreshQuote is the regression
// test for H3: CompleteRIExchangeWithPayment must be called with the amount
// AWS confirmed during Execute (freshQ.PaymentDueUSDStr), not the stale
// record.PaymentDue stored from the pre-execution quote.
func TestExecuteApprovedExchange_AcceptedAmountFromFreshQuote(t *testing.T) {
	ctx := context.Background()
	const id = "550e8400-e29b-41d4-a716-000000000013"

	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })

	mockStore.On("ReserveApprovedRIExchange", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return("500.000000", nil)
	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{
		RIExchangeMaxDailyUSD:       1000,
		RIExchangeMaxPerExchangeUSD: 500,
	}, nil)

	// Execute returns a fresh quote ($35.50) different from pre-execution ($30.00).
	h := &Handler{
		config: mockStore,
		executeExchangeFn: func(_ context.Context, _ exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
			return "exch-h3-fresh", &exchange.ExchangeQuoteSummary{
				PaymentDueUSDStr: "35.500000",
			}, nil
		},
	}

	// The FRESH amount "35.500000" must reach CompleteRIExchangeWithPayment,
	// not the stale "30.00" from record.PaymentDue.
	mockStore.On("CompleteRIExchangeWithPayment", mock.Anything, id, "exch-h3-fresh", "35.500000").Return(nil)

	record := &config.RIExchangeRecord{
		ID:               id,
		Region:           "us-east-1",
		SourceRIIDs:      []string{"ri-1"},
		TargetOfferingID: "offering-1",
		TargetCount:      1,
		PaymentDue:       "30.00", // stale pre-execution quote
	}

	resp, err := h.executeApprovedExchange(ctx, id, record)
	require.NoError(t, err)
	respMap, ok := resp.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "completed", respMap["status"])
}

// ─── H4: persistent ledger failure returns error ──────────────────────────────

// TestExecuteApprovedExchange_LedgerWriteFailure_ReturnsError is the regression
// test for H4: when CompleteRIExchangeWithPayment fails persistently (all retry
// attempts exhausted) after money has already moved, executeApprovedExchange must
// return an error rather than silently logging and returning a success response.
// The caller (approveRIExchange) must see this as an error so it can return
// HTTP 500 and the operator knows the ledger is inconsistent.
func TestExecuteApprovedExchange_LedgerWriteFailure_ReturnsError(t *testing.T) {
	ctx := context.Background()
	const id = "550e8400-e29b-41d4-a716-000000000014"

	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })

	mockStore.On("ReserveApprovedRIExchange", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return("500.000000", nil)
	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{
		RIExchangeMaxDailyUSD:       1000,
		RIExchangeMaxPerExchangeUSD: 500,
	}, nil)

	h := &Handler{
		config: mockStore,
		executeExchangeFn: func(_ context.Context, _ exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
			return "exch-h4-test", &exchange.ExchangeQuoteSummary{PaymentDueUSDStr: "0"}, nil
		},
	}

	// Fail all three retry attempts.
	mockStore.On("CompleteRIExchangeWithPayment", mock.Anything, id, "exch-h4-test", "0").
		Return(fmt.Errorf("DB write failed")).Times(3)

	record := &config.RIExchangeRecord{
		ID:               id,
		Region:           "us-east-1",
		SourceRIIDs:      []string{"ri-1"},
		TargetOfferingID: "offering-1",
		TargetCount:      1,
		PaymentDue:       "0",
	}

	_, err := h.executeApprovedExchange(ctx, id, record)
	require.Error(t, err, "persistent ledger failure must propagate as a non-nil error")
	assert.Contains(t, err.Error(), "ledger update failed",
		"error must describe the ledger failure for operator triage")
	assert.Contains(t, err.Error(), "exch-h4-test",
		"error must include the exchange ID for operator correlation with AWS")
}

// fourEyesExchangeBody is a valid AWS execute body used by the 4-eyes gate tests.
const fourEyesExchangeBody = `{"ri_ids":["ri-123"],"target_offering_id":"off-1","target_count":1,"max_payment_due_usd":"250.50","region":"eu-central-1"}`

// newFourEyesExecuteExchangeHandler builds an AWS handler whose session clears
// permission, account scope and constraints, so the only remaining gate before
// any exchange side effect is the 4-eyes policy under test.
func newFourEyesExecuteExchangeHandler(t *testing.T, store *MockConfigStore) *Handler {
	t.Helper()
	ctx := context.Background()
	const deploymentAccountID = "11111111-2222-3333-4444-555555555555"

	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("ValidateSession", ctx, "exchange-token").Return(&Session{UserID: "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"}, nil)
	mockAuth.On("HasPermissionAPI", ctx, "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee", "execute", "ri-exchange").Return(true, nil)
	allowAnyAccountScope(mockAuth)
	mockAuth.On("HasPermissionForConstraintsAPI", ctx, "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee", "execute", "ri-exchange", mock.Anything).Return(true, nil)

	return &Handler{
		auth:   mockAuth,
		config: store,
		reshapeAccountResolver: func(_ context.Context) (string, error) {
			return deploymentAccountID, nil
		},
	}
}

// spyOnAWSExchange installs an executeExchangeFn that counts calls and
// reports success, so a 4-eyes test asserts that no exchange was submitted.
// The real executor would fail on missing AWS credentials, so an error alone
// would not prove the gate held (issue #679).
func spyOnAWSExchange(h *Handler) *int {
	calls := 0
	h.executeExchangeFn = func(context.Context, exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
		calls++
		return "exch-must-not-run", &exchange.ExchangeQuoteSummary{PaymentDueUSDStr: "0"}, nil
	}
	return &calls
}

// TestExecuteExchange_FourEyesRefusesDirectExecute pins issue #517: with
// RequireDifferentApprover on, the direct AWS execute endpoint must refuse
// before any exchange is attempted. A spy executor records any exchange call.
func TestExecuteExchange_FourEyesRefusesDirectExecute(t *testing.T) {
	ctx := context.Background()
	store := &MockConfigStore{}
	t.Cleanup(func() { store.AssertExpectations(t) })
	store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{RequireDifferentApprover: true}, nil)
	h := newFourEyesExecuteExchangeHandler(t, store)
	exchangeCalls := spyOnAWSExchange(h)

	_, err := h.executeExchange(ctx, &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer exchange-token"},
		Body:    fourEyesExchangeBody,
	})
	assert.Equal(t, 0, *exchangeCalls, "no exchange may be submitted while 4-eyes is on")
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a ClientError, got: %v", err)
	assert.Equal(t, 403, ce.code)
	assert.Equal(t, "direct RI exchange execution is disabled while 4-eyes approval mode is on; use the approval flow", ce.Error())
}

// TestExecuteExchange_FourEyesPolicyLookupErrorFailsClosed: an unreadable
// global config must not be treated as "4-eyes off".
func TestExecuteExchange_FourEyesPolicyLookupErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	store := &MockConfigStore{}
	t.Cleanup(func() { store.AssertExpectations(t) })
	store.On("GetGlobalConfig", mock.Anything).Return(nil, errors.New("db down"))
	h := newFourEyesExecuteExchangeHandler(t, store)
	exchangeCalls := spyOnAWSExchange(h)

	_, err := h.executeExchange(ctx, &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer exchange-token"},
		Body:    fourEyesExchangeBody,
	})
	assert.Equal(t, 0, *exchangeCalls, "an unreadable 4-eyes policy must not let the exchange run")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "4-eyes policy check")
	_, isClient := IsClientError(err)
	assert.False(t, isClient, "a lookup failure is a server error, not a client refusal")
}

// TestExecuteApprovedExchange_ReservationRefused_NoExecute asserts that when the
// atomic reservation refuses (daily headroom gone), the exchange fails and the
// provider is never called (#656).
func TestExecuteApprovedExchange_ReservationRefused_NoExecute(t *testing.T) {
	ctx := context.Background()
	const id = "550e8400-e29b-41d4-a716-000000000656"

	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{
		RIExchangeMaxDailyUSD: 1000, RIExchangeMaxPerExchangeUSD: 500,
	}, nil)
	mockStore.On("ReserveApprovedRIExchange", ctx, id, "1000.000000", "500.000000").
		Return("", fmt.Errorf("%w: initial 600.000000, remaining 400.000000", config.ErrRIExchangeDailyCapExceeded))
	mockStore.On("FailRIExchange", mock.Anything, id, mock.MatchedBy(func(r string) bool {
		return strings.Contains(r, "daily cap exceeded")
	})).Return(nil)

	called := false
	h := &Handler{config: mockStore, executeExchangeFn: func(context.Context, exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
		called = true
		return "", nil, nil
	}}
	resp, err := h.executeApprovedExchange(ctx, id, &config.RIExchangeRecord{
		ID: id, Region: "us-east-1", SourceRIIDs: []string{"ri-1"}, TargetOfferingID: "o", TargetCount: 1, PaymentDue: "600.00",
	})
	require.NoError(t, err)
	assert.Equal(t, "failed", resp.(map[string]any)["status"])
	assert.False(t, called, "provider must not be called when the reservation is refused")
}

// TestExecuteApprovedExchange_SettlesAcceptedAmountWithinReservedCeiling asserts
// the full reserved ceiling (not the initial quote) bounds Execute, and the fresh
// accepted amount is what gets settled on the same row (#656).
func TestExecuteApprovedExchange_SettlesAcceptedAmountWithinReservedCeiling(t *testing.T) {
	ctx := context.Background()
	const id = "550e8400-e29b-41d4-a716-000000000657"

	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{
		RIExchangeMaxDailyUSD: 1000, RIExchangeMaxPerExchangeUSD: 500,
	}, nil)
	mockStore.On("ReserveApprovedRIExchange", ctx, id, "1000.000000", "500.000000").Return("400.000000", nil)
	mockStore.On("CompleteRIExchangeWithPayment", mock.Anything, id, "exch-1", "390.000000").Return(nil)

	var got exchange.ExchangeExecuteRequest
	h := &Handler{config: mockStore, executeExchangeFn: func(_ context.Context, req exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
		got = req
		return "exch-1", &exchange.ExchangeQuoteSummary{PaymentDueUSDStr: "390.000000"}, nil
	}}
	_, err := h.executeApprovedExchange(ctx, id, &config.RIExchangeRecord{
		ID: id, Region: "us-east-1", SourceRIIDs: []string{"ri-1"}, TargetOfferingID: "o", TargetCount: 1, PaymentDue: "100.00",
	})
	require.NoError(t, err)
	require.NotNil(t, got.MaxPaymentDueUSD)
	assert.Equal(t, 0, got.MaxPaymentDueUSD.Cmp(big.NewRat(400, 1)), "ceiling must be the reserved $400, not the $100 initial quote")
}

// TestExecuteApprovedExchange_ReservationInfrastructureError_HidesDetail asserts
// that a non-cap reservation error (lock/DB failure) is logged, not echoed to the
// approver, and the provider is never called.
func TestExecuteApprovedExchange_ReservationInfrastructureError_HidesDetail(t *testing.T) {
	ctx := context.Background()
	const id = "550e8400-e29b-41d4-a716-000000000658"

	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{
		RIExchangeMaxDailyUSD: 1000, RIExchangeMaxPerExchangeUSD: 500,
	}, nil)
	mockStore.On("ReserveApprovedRIExchange", ctx, id, "1000.000000", "500.000000").
		Return("", errors.New("failed to lock RI exchange daily spend: connection reset by peer"))
	mockStore.On("FailRIExchange", mock.Anything, id, "daily spending cap check failed").Return(nil)

	called := false
	h := &Handler{config: mockStore, executeExchangeFn: func(context.Context, exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
		called = true
		return "", nil, nil
	}}
	resp, err := h.executeApprovedExchange(ctx, id, &config.RIExchangeRecord{
		ID: id, Region: "us-east-1", SourceRIIDs: []string{"ri-1"}, TargetOfferingID: "o", TargetCount: 1, PaymentDue: "10.00",
	})
	require.NoError(t, err)
	assert.Equal(t, "daily spending cap check failed", resp.(map[string]any)["reason"])
	assert.False(t, called)
}

// A convertible RI with a non-positive Duration has no computable monthly
// cost. Converting it to MonthlyCost=0 would make reshape's pricing gate
// treat it as free and skip the dollar-units check, so it must be excluded.
func TestConvertToExchangeTypes_ExcludesNonPositiveDuration(t *testing.T) {
	valid := ec2svc.ConvertibleRI{
		ReservedInstanceID: "ri-valid", InstanceType: "m5.large", InstanceCount: 2,
		NormalizationFactor: 4, FixedPrice: 8760, UsagePrice: 0.01, Duration: 31536000, CurrencyCode: "USD",
	}
	instances := []ec2svc.ConvertibleRI{
		{ReservedInstanceID: "ri-zero", InstanceType: "m5.large", InstanceCount: 1, FixedPrice: 8760, Duration: 0},
		valid,
		{ReservedInstanceID: "ri-neg", InstanceType: "c5.large", InstanceCount: 1, FixedPrice: 8760, Duration: -1},
	}

	riInfos, _, excluded := convertToExchangeTypes(instances, nil)

	require.Len(t, riInfos, 1)
	assert.Equal(t, "ri-valid", riInfos[0].ID)
	assert.InDelta(t, (8760.0/8760+0.01)*730, riInfos[0].MonthlyCost, 1e-6)
	assert.Equal(t, int64(31536000), riInfos[0].TermSeconds)
	require.Len(t, excluded, 2)
	assert.Equal(t, "ri-zero", excluded[0].RIID)
	assert.Equal(t, "ri-neg", excluded[1].RIID)
	assert.Contains(t, excluded[0].Reason, "term length")
}

// The reshape response must name RIs left out of cost-based analysis so the
// UI can show them instead of silently giving them no recommendation.
func TestGetReshapeRecommendations_ReportsExcludedRIs(t *testing.T) {
	mockStore := &MockConfigStore{}
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockStore.On("ListStoredRecommendations", mock.Anything, mock.Anything).
		Return([]config.RecommendationRecord(nil), nil)
	h := &Handler{
		config: mockStore,
		auth:   &mockAuthForExchange{},
		reshapeEC2Factory: func(_ aws.Config) reshapeEC2Client {
			return &fakeReshapeEC2Stub{instances: []ec2svc.ConvertibleRI{
				{ReservedInstanceID: "ri-bad", InstanceType: "m5.xlarge", InstanceCount: 1, FixedPrice: 8760, Duration: 0, CurrencyCode: "USD"},
				{ReservedInstanceID: "ri-ok", InstanceType: "m5.xlarge", InstanceCount: 1, FixedPrice: 8760, Duration: 31536000, CurrencyCode: "USD"},
			}}
		},
		reshapeRecsFactory: func(_ aws.Config) reshapeRecsClient {
			return &fakeReshapeRecsStub{utilization: []recommendations.RIUtilization{
				{ReservedInstanceID: "ri-bad", UtilizationPercent: 50.0},
				{ReservedInstanceID: "ri-ok", UtilizationPercent: 50.0},
			}}
		},
		reshapeAccountResolver: func(_ context.Context) (string, error) { return "", nil },
	}
	h.awsCfgOnce.Do(func() { h.awsCfg = aws.Config{Region: "us-east-1"} })

	req := &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"authorization": "Bearer test-token"},
		QueryStringParameters: map[string]string{"lookback_days": "30", "threshold": "95.0"},
	}
	out, err := h.getReshapeRecommendations(context.Background(), req)
	require.NoError(t, err)
	resp, ok := out.(*ReshapeRecommendationsResponse)
	require.True(t, ok)

	require.Len(t, resp.ExcludedRIs, 1)
	assert.Equal(t, "ri-bad", resp.ExcludedRIs[0].RIID)
	assert.NotEmpty(t, resp.ExcludedRIs[0].Reason)
	require.NotEmpty(t, resp.Recommendations)
	for _, r := range resp.Recommendations {
		assert.NotEqual(t, "ri-bad", r.SourceRIID)
	}
	assert.Contains(t, resp.Recommendations[0].SourceRIID, "ri-ok")

	raw, err := json.Marshal(resp)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"excluded_ris":[{"ri_id":"ri-bad"`)
}

func TestMonthlyCostFromConvertibleRI_NonPositiveDurationErrors(t *testing.T) {
	for _, d := range []int64{0, -1} {
		_, err := monthlyCostFromConvertibleRI(ec2svc.ConvertibleRI{ReservedInstanceID: "ri-x", FixedPrice: 100, Duration: d})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ri-x")
	}
	cost, err := monthlyCostFromConvertibleRI(ec2svc.ConvertibleRI{FixedPrice: 8760, Duration: 31536000})
	require.NoError(t, err)
	assert.InDelta(t, 730.0, cost, 1e-6)
}

// A convertible RI whose FixedPrice, UsagePrice and RecurringHourlyAmount are
// all zero has no pricing data; it is not free. MonthlyCost=0 would make
// reshape's pricing gate pass every alternative and score the RI as neutral,
// so it must be excluded (issue #1188). A No Upfront RI (FixedPrice 0 with an
// hourly charge) is real pricing and must be kept.
func TestConvertToExchangeTypes_ExcludesRIWithoutPricing(t *testing.T) {
	const oneYear = 31536000
	instances := []ec2svc.ConvertibleRI{
		{ReservedInstanceID: "ri-unpriced", InstanceType: "m5.large", InstanceCount: 1, Duration: oneYear},
		{ReservedInstanceID: "ri-no-upfront", InstanceType: "m5.large", InstanceCount: 1, RecurringHourlyAmount: 0.05, Duration: oneYear},
		{ReservedInstanceID: "ri-usage-only", InstanceType: "m5.large", InstanceCount: 1, UsagePrice: 0.04, Duration: oneYear},
		{ReservedInstanceID: "ri-all-upfront", InstanceType: "m5.large", InstanceCount: 1, FixedPrice: 876, Duration: oneYear},
		{ReservedInstanceID: "ri-negative", InstanceType: "c5.large", InstanceCount: 1, FixedPrice: -876, Duration: oneYear},
	}

	riInfos, _, excluded := convertToExchangeTypes(instances, nil)

	require.Len(t, riInfos, 3)
	want := map[string]float64{"ri-no-upfront": 0.05 * 730, "ri-usage-only": 0.04 * 730, "ri-all-upfront": 876.0 / 8760 * 730}
	for _, ri := range riInfos {
		cost, ok := want[ri.ID]
		require.True(t, ok, "unexpected RI %s kept", ri.ID)
		assert.InDelta(t, cost, ri.MonthlyCost, 1e-6, ri.ID)
		assert.Greater(t, ri.MonthlyCost, 0.0, ri.ID)
	}
	require.Len(t, excluded, 2)
	assert.Equal(t, "ri-unpriced", excluded[0].RIID)
	assert.Equal(t, "ri-negative", excluded[1].RIID)
	for _, e := range excluded {
		assert.Contains(t, e.Reason, "no pricing")
		assert.NotContains(t, e.Reason, "term length")
	}
}

// End to end through the reshape handler: an unpriced RI is reported in
// excluded_ris with the pricing reason and gets no recommendation, while a
// priced RI in the same account still does.
func TestGetReshapeRecommendations_ReportsUnpricedRIs(t *testing.T) {
	mockStore := &MockConfigStore{}
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockStore.On("ListStoredRecommendations", mock.Anything, mock.Anything).
		Return([]config.RecommendationRecord(nil), nil)
	h := &Handler{
		config: mockStore,
		auth:   &mockAuthForExchange{},
		reshapeEC2Factory: func(_ aws.Config) reshapeEC2Client {
			return &fakeReshapeEC2Stub{instances: []ec2svc.ConvertibleRI{
				{ReservedInstanceID: "ri-unpriced", InstanceType: "m5.xlarge", InstanceCount: 1, Duration: 31536000, CurrencyCode: "USD"},
				{ReservedInstanceID: "ri-ok", InstanceType: "m5.xlarge", InstanceCount: 1, FixedPrice: 8760, Duration: 31536000, CurrencyCode: "USD"},
			}}
		},
		reshapeRecsFactory: func(_ aws.Config) reshapeRecsClient {
			return &fakeReshapeRecsStub{utilization: []recommendations.RIUtilization{
				{ReservedInstanceID: "ri-unpriced", UtilizationPercent: 50.0},
				{ReservedInstanceID: "ri-ok", UtilizationPercent: 50.0},
			}}
		},
		reshapeAccountResolver: func(_ context.Context) (string, error) { return "", nil },
	}
	h.awsCfgOnce.Do(func() { h.awsCfg = aws.Config{Region: "us-east-1"} })

	req := &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"authorization": "Bearer test-token"},
		QueryStringParameters: map[string]string{"lookback_days": "30", "threshold": "95.0"},
	}
	out, err := h.getReshapeRecommendations(context.Background(), req)
	require.NoError(t, err)
	resp, ok := out.(*ReshapeRecommendationsResponse)
	require.True(t, ok)

	require.Len(t, resp.ExcludedRIs, 1)
	assert.Equal(t, "ri-unpriced", resp.ExcludedRIs[0].RIID)
	assert.Contains(t, resp.ExcludedRIs[0].Reason, "no pricing")
	require.NotEmpty(t, resp.Recommendations)
	for _, r := range resp.Recommendations {
		assert.NotEqual(t, "ri-unpriced", r.SourceRIID)
	}
}

func TestMonthlyCostFromConvertibleRI_NoPositivePriceErrors(t *testing.T) {
	for name, ri := range map[string]ec2svc.ConvertibleRI{
		"all zero":     {ReservedInstanceID: "ri-x", Duration: 31536000},
		"negative sum": {ReservedInstanceID: "ri-x", FixedPrice: -876, Duration: 31536000},
		"NaN":          {ReservedInstanceID: "ri-x", UsagePrice: math.NaN(), Duration: 31536000},
	} {
		cost, err := monthlyCostFromConvertibleRI(ri)
		require.Error(t, err, name)
		assert.Zero(t, cost, name)
		assert.Contains(t, err.Error(), "ri-x", name)
		assert.ErrorIs(t, err, ricost.ErrNoPricing, name)
		assert.NotErrorIs(t, err, ricost.ErrNonPositiveDuration, name)
	}
	_, err := monthlyCostFromConvertibleRI(ec2svc.ConvertibleRI{ReservedInstanceID: "ri-y"})
	assert.ErrorIs(t, err, ricost.ErrNonPositiveDuration, "duration is checked before pricing")
	cost, err := monthlyCostFromConvertibleRI(ec2svc.ConvertibleRI{RecurringHourlyAmount: 0.05, Duration: 31536000})
	require.NoError(t, err)
	assert.InDelta(t, 36.5, cost, 1e-6)
}
