package api

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// A token-path approver is the contact of one account on the execution, so a
// failure body must not carry run-error text that names the other accounts.

const (
	otherAcctID   = "999988887777"
	otherAcctName = "Beta Corp Production"
)

var errOtherAcct = fmt.Errorf("account %s (%s): assume role arn:aws:iam::%s:role/x failed", otherAcctID, otherAcctName, otherAcctID)

type tokenFailureCase struct {
	name   string
	status string // the row's status; also the expected details status
	err    error
	code   int
}

func tokenFailureCases() []tokenFailureCase {
	refusal := func(msg string) error {
		return fmt.Errorf("%w: %w", purchase.ErrClaimedRowRefused, NewClientError(403, msg))
	}
	return []tokenFailureCase{
		{"audit loss", config.StatusPartiallyCompleted, fmt.Errorf("%w: terminal save failed; original execution error: %w", config.ErrAuditLoss, errOtherAcct), 500},
		{"partial run", config.StatusPartiallyCompleted, fmt.Errorf("some purchases failed: %w", errOtherAcct), 502},
		{"default failure", "failed", fmt.Errorf("provider call failed: %w", errOtherAcct), 502},
		{"post-claim step cap", "failed", refusal(fmt.Sprintf("step total commitment 9000.00 exceeds max_step_commitment 100.00 (%s, %s)", otherAcctID, otherAcctName)), 403},
		{"post-claim ladder (plain error)", "failed", fmt.Errorf("ladder tranche for %s (%s) no longer matches", otherAcctID, otherAcctName), 502},
		{"stale Azure pricing", "failed", fmt.Errorf("%w: Azure recommendation %q no longer has the approved price", purchase.ErrStaleAzurePricing, "azure|"+otherAcctID+"|compute|eastus|D2s|"), 409},
		{"audit loss wrapping stale pricing", "failed", fmt.Errorf("%w: terminal save failed (dial tcp 10.0.0.5:5432); original execution error: %w", config.ErrAuditLoss, fmt.Errorf("%w: Azure recommendation %q", purchase.ErrStaleAzurePricing, "azure|"+otherAcctID+"|compute|eastus|D2s|")), 500},
	}
}

func multiAcctRow(execID, status string) *config.PurchaseExecution {
	row := partialRunRow(execID)
	row.Status = status
	other := otherAcctID
	row.Recommendations[1].CloudAccountID = &other
	return row
}

// spanTwoAccounts makes the authorized execution span two accounts with
// different contacts; the approver is the contact of the first only.
func spanTwoAccounts(t *testing.T, store *MockConfigStore, approver string) {
	t.Helper()
	other := otherAcctID
	found := false
	for _, c := range store.ExpectedCalls {
		if c.Method != "GetExecutionByID" {
			continue
		}
		exec := c.ReturnArguments.Get(0).(*config.PurchaseExecution)
		exec.Recommendations = append(exec.Recommendations, config.RecommendationRecord{ID: "r2", Provider: "aws", Service: "ec2", Region: "us-east-1", CloudAccountID: &other})
		found = true
	}
	require.True(t, found)
	store.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
		if id == otherAcctID {
			return &config.CloudAccount{ID: id, Name: otherAcctName, ContactEmail: "beta-owner@example.com"}, nil
		}
		return &config.CloudAccount{ID: id, ContactEmail: approver}, nil
	}
}

func TestTokenApprove_FailureBodyOmitsRunErrorText(t *testing.T) {
	ctx := context.Background()
	execID := "b1234567-b123-b123-b123-b1234567abcd"
	for _, tc := range tokenFailureCases() {
		t.Run(tc.name, func(t *testing.T) {
			handler, _, mockConfig := tokenApproveHandler(t, ctx, execID, "contact@example.com", multiAcctRow(execID, tc.status), "", tc.err)
			spanTwoAccounts(t, mockConfig, "contact@example.com")
			_, err := handler.approvePurchase(ctx, &events.LambdaFunctionURLRequest{Headers: map[string]string{"authorization": "Bearer sess-tok"}}, execID, "valid-token")
			ce, ok := IsClientError(err)
			require.True(t, ok, "expected a client error, got %v", err)
			assert.Equal(t, tc.code, ce.code)
			body := fmt.Sprintf("%s %v", ce.message, ce.Details())
			assert.NotContains(t, body, otherAcctID)
			assert.NotContains(t, body, otherAcctName)
			assert.NotContains(t, body, "10.0.0.5")
			if tc.code != 500 {
				assert.Equal(t, execID, ce.Details()["execution_id"])
				assert.Equal(t, tc.status, ce.Details()["status"])
			}
		})
	}
}

// The session caller holds approve-* scoped to the execution and keeps the
// detailed message.
func TestSessionApprove_FailureKeepsRunErrorText(t *testing.T) {
	ctx := context.Background()
	execID := "c1234567-c123-c123-c123-c1234567abcd"
	for _, tc := range tokenFailureCases()[:3] {
		t.Run(tc.name, func(t *testing.T) {
			handler, _ := sessionApproveHandler(t, ctx, execID, "contact@example.com", multiAcctRow(execID, tc.status), "", tc.err)
			_, err := handler.approvePurchase(ctx, &events.LambdaFunctionURLRequest{Headers: map[string]string{"authorization": "Bearer sess-tok"}}, execID, "")
			ce, ok := IsClientError(err)
			require.True(t, ok, "expected a client error, got %v", err)
			assert.Equal(t, tc.code, ce.code)
			assert.Contains(t, ce.message, otherAcctName)
		})
	}
}

// The delayed token approve answers a store failure without its text.
func TestTokenApprove_DelayedScheduleFailureOmitsStoreText(t *testing.T) {
	future := time.Now().Add(time.Hour)
	exec := &config.PurchaseExecution{Status: "pending", ApprovalToken: config.HashApprovalToken(tokenErrRawToken), ApprovalTokenExpiresAt: &future}
	h := delayTokenHandler(t, exec)
	store := h.config.(*MockConfigStore)
	store.On("TransitionExecutionStatusTokenPinned", mock.Anything, tokenErrExecID, mock.Anything, "scheduled", mock.Anything, tokenErrRawToken).
		Return(nil, errors.New("pq: connection to 10.0.0.5:5432 refused")).Once()

	resp, err := h.HandleRequest(context.Background(), tokenErrRequest("approve", tokenErrRawToken))
	require.NoError(t, err)
	assert.Equal(t, 500, resp.StatusCode, resp.Body)
	assert.NotContains(t, resp.Body, "10.0.0.5")
}
