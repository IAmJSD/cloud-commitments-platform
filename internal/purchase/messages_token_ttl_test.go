package purchase

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #268: loadAsyncExecutionForApproval was a third copy of the
// approval-token check (alongside ValidateApprovalToken and
// loadCancelableExecution) that compared the token but never enforced
// ApprovalTokenExpiresAt. An expired token on an SQS approve/cancel message
// was only rejected because ApproveExecution / CancelExecution re-validated
// it one frame later. These tests pin the TTL guard on the helper itself and
// on the SQS entry points, before approver resolution runs.

func asyncTTLExecution(id string, expiresAt *time.Time) *config.PurchaseExecution {
	accountID := "acct-1"
	return &config.PurchaseExecution{
		ExecutionID:            id,
		Status:                 "pending",
		ApprovalToken:          config.HashApprovalToken("correct-token"),
		ApprovalTokenExpiresAt: expiresAt,
		Recommendations: []config.RecommendationRecord{
			{CloudAccountID: &accountID},
		},
	}
}

func TestLoadAsyncExecutionForApproval_TokenValidation(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)

	tests := []struct {
		name      string
		expiresAt *time.Time
		token     string
		wantErr   error
	}{
		{name: "expired token is rejected", expiresAt: &past, token: "correct-token", wantErr: ErrApprovalTokenExpired},
		{name: "unexpired token is accepted", expiresAt: &future, token: "correct-token"},
		{name: "legacy row without expiry is accepted", expiresAt: nil, token: "correct-token"},
		{name: "wrong token is rejected", expiresAt: &future, token: "wrong-token", wantErr: ErrInvalidApprovalToken},
		{name: "wrong token on expired row reports invalid, not expired", expiresAt: &past, token: "wrong-token", wantErr: ErrInvalidApprovalToken},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			mockStore := new(MockConfigStore)
			exec := asyncTTLExecution("exec-ttl", tt.expiresAt)
			mockStore.On("GetExecutionByID", ctx, "exec-ttl").Return(exec, nil).Once()

			manager := &Manager{config: mockStore}
			got, err := manager.loadAsyncExecutionForApproval(ctx, &AsyncMessage{
				ExecutionID: "exec-ttl",
				Token:       tt.token,
			})

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				assert.Nil(t, got)
			} else {
				require.NoError(t, err)
				assert.Same(t, exec, got)
			}
			mockStore.AssertExpectations(t)
		})
	}
}

// TestProcessMessage_ExpiredTokenRejectedBeforeApproverLookup drives the
// real SQS entry point for both approve and cancel with an expired token
// from an actor who IS on the approver list. The rejection must come from
// verifyAsyncApprovalActor's own token check: the execution is loaded once
// and approver resolution (GetCloudAccount) never runs. Before the fix the
// approver lookup succeeded and the message was only rejected by the second
// load inside ApproveExecution / CancelExecution.
func TestProcessMessage_ExpiredTokenRejectedBeforeApproverLookup(t *testing.T) {
	for _, msgType := range []string{"approve", "cancel"} {
		t.Run(msgType, func(t *testing.T) {
			ctx := context.Background()
			mockStore := new(MockConfigStore)
			mockEmail := new(MockEmailSender)

			past := time.Now().Add(-24 * time.Hour)
			exec := asyncTTLExecution("exec-expired", &past)
			// GetExecutionByID and GetCloudAccount are registered without a
			// call limit so a pre-fix run reaches the approver match and the
			// second load inside ApproveExecution / CancelExecution instead
			// of panicking on an unexpected call; the call-count and
			// AssertNotCalled checks below are the actual assertions.
			mockStore.On("GetExecutionByID", ctx, "exec-expired").Return(exec, nil)
			account := &config.CloudAccount{ID: "acct-1", ContactEmail: "owner@example.com"}
			mockStore.On("GetCloudAccount", ctx, "acct-1").Return(account, nil).Maybe()

			manager := &Manager{
				config:       mockStore,
				email:        mockEmail,
				dashboardURL: "https://dashboard.example.com",
			}

			err := manager.ProcessMessage(ctx, `{"type":"`+msgType+`","execution_id":"exec-expired","token":"correct-token","actor_email":"owner@example.com"}`)
			require.ErrorIs(t, err, ErrApprovalTokenExpired)
			mockStore.AssertNumberOfCalls(t, "GetExecutionByID", 1)
			mockStore.AssertNotCalled(t, "GetCloudAccount", mock.Anything, mock.Anything)
			mockStore.AssertNotCalled(t, "TransitionExecutionStatus",
				mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			mockStore.AssertNotCalled(t, "CancelExecutionAtomic",
				mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			mockStore.AssertNotCalled(t, "SavePurchaseExecution", mock.Anything, mock.Anything)
			mockStore.AssertExpectations(t)
		})
	}
}
