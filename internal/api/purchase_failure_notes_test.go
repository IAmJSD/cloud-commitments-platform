package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fakeConnectErr = "failed to connect to `host=db.internal.example user=cudly_app database=cudly`: " +
	"dial error (dial tcp 10.0.0.5:5432: connect: connection refused)"

func assertNoConnectText(t *testing.T, where, s string) {
	t.Helper()
	for _, n := range []string{"db.internal.example", "cudly_app", "10.0.0.5", "dial tcp"} {
		assert.NotContains(t, s, n, where)
	}
}

func TestTailedNotesWithStoreErrors_NeverReachUserSurfaces(t *testing.T) {
	notes := []string{
		"ramp not advanced: " + fakeConnectErr,
		"refusing to execute: ladder tranche lookup failed: " + fakeConnectErr,
		"refusing to execute ladder purchase: global config unreadable: " + fakeConnectErr,
		purchase.ErrStaleAzurePricing.Error() + ": cannot read the latest collection state to confirm Azure prices are current: " + fakeConnectErr,
	}
	for _, note := range notes {
		name := note[:strings.Index(note, ":")]
		t.Run(name, func(t *testing.T) {
			row := executionToHistoryRow(config.PurchaseExecution{ExecutionID: "e-1", Status: "failed", Error: note}, "", "")
			assertNoConnectText(t, "status_description", row.StatusDescription)
			assert.NotEmpty(t, row.StatusDescription)

			exec := &config.PurchaseExecution{ExecutionID: "e-1", Status: "failed", Error: note}
			body, err := json.Marshal(buildPurchaseDetailsResponse(exec, "plan"))
			require.NoError(t, err)
			assertNoConnectText(t, "details", string(body))

			creator := retryCallerID
			failed := &config.PurchaseExecution{ExecutionID: retryExecID, Status: "failed", CreatedByUserID: &creator,
				Error: "FROM_EMAIL not configured for this deployment; " + note}
			handler, _, _ := buildSessionRetryHandler(t, failed, &Session{UserID: retryCallerID}, false, true)
			_, rErr := handler.retryPurchase(t.Context(), sessionRetryReq(), retryExecID)
			ce, ok := IsClientError(rErr)
			require.True(t, ok)
			assert.Equal(t, 409, ce.code)
			raw, mErr := json.Marshal(ce.Details())
			require.NoError(t, mErr)
			assertNoConnectText(t, "retry 409", string(raw)+ce.Error())
			assert.NotEmpty(t, ce.Details()["failure_reason"])
		})
	}
}

func TestExecuteFailure_SentinelWrappingProviderErrorShowsOnlySentinelText(t *testing.T) {
	for _, sentinel := range []error{purchase.ErrStaleAzurePricing, purchase.ErrClaimedRowRefused} {
		err := fmt.Errorf("%w: %w", sentinel, errors.New(fakeConnectErr))
		_, msg := summarizeExecuteFailure("e-1", err)
		assert.Equal(t, sentinel.Error(), msg)
		out := executeFailureError("e-1", "executed", &config.PurchaseExecution{Status: "failed"}, err)
		require.Error(t, out)
		// Non-client errors render as a generic 500 (handler.go), so only
		// structured client errors can carry text to the caller.
		if ce, ok := IsClientError(out); ok {
			assertNoConnectText(t, "live", ce.Error())
		}
	}
}

func TestTokenActionError_StalePricingShowsOnlySentinelText(t *testing.T) {
	err := tokenActionError(fmt.Errorf("%w: %w", purchase.ErrStaleAzurePricing, errors.New(fakeConnectErr)))
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, purchase.ErrStaleAzurePricing.Error(), ce.Error())
}

// A refused AWS purchase keeps ops_hint (the History UI hides Retry on it) but
// carries no duplicate-purchase warning.
func TestHistoryRow_DefiniteAWSRefusalKeepsOpsHintWithoutDuplicateWarning(t *testing.T) {
	denied := "m5.large: purchase failed: failed to purchase EC2 RI: operation error EC2: X, https response error StatusCode: 403, RequestID: r, api error UnauthorizedOperation: msg"
	row := executionToHistoryRow(config.PurchaseExecution{
		ExecutionID: "e-1", Status: "failed", Error: denied,
		Recommendations: []config.RecommendationRecord{{Provider: "aws", Service: "ec2"}},
	}, "", "")
	assert.True(t, row.RedriveUnsafe)
	assert.NotEmpty(t, row.OpsHint)
	assert.NotContains(t, strings.ToLower(row.OpsHint), "duplicate guard")
	assert.NotEqual(t, purchase.RedriveRefusalReason(&config.PurchaseExecution{
		Recommendations: []config.RecommendationRecord{{Provider: "aws", Service: "ec2"}}}), row.OpsHint)
}
