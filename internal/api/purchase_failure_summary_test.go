package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	fakeEncodedBlob = "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789LongEncodedBlob"
	fakeAWSDenied   = "operation error EC2: PurchaseReservedInstancesOffering, https response error StatusCode: 403, " +
		"RequestID: 11111111-2222-3333-4444-555555555555, api error UnauthorizedOperation: You are not authorized. " +
		"User: arn:aws:sts::123456789012:assumed-role/cudly-role/session. " +
		"Encoded authorization failure message: " + fakeEncodedBlob
)

var providerTextNeedles = []string{"arn:aws", "123456789012", "Encoded authorization", fakeEncodedBlob, "RequestID"}

func assertNoProviderText(t *testing.T, where, s string) {
	t.Helper()
	for _, n := range providerTextNeedles {
		assert.NotContains(t, s, n, where)
	}
}

// One realistic provider denial must stay out of every user-facing surface.
func TestProviderDenial_NeverReachesUserSurfaces(t *testing.T) {
	const execID = "11111111-1111-1111-1111-111111111111"
	raw := errors.New(fakeAWSDenied)
	rec := config.RecommendationRecord{Provider: "aws", Service: "rds", Error: fakeAWSDenied}

	t.Run("direct execute 502", func(t *testing.T) {
		err := executeFailureError(execID, "executed", &config.PurchaseExecution{Status: "failed"}, raw)
		ce, ok := IsClientError(err)
		require.True(t, ok)
		assert.Equal(t, 502, ce.code)
		body, mErr := json.Marshal(ce.Details())
		require.NoError(t, mErr)
		assertNoProviderText(t, "message", ce.Error())
		assertNoProviderText(t, "details", string(body))
		assert.Contains(t, ce.Error(), execID)
		assert.Equal(t, "access_denied", ce.Details()["category"])
	})

	t.Run("history failed row", func(t *testing.T) {
		row := executionToHistoryRow(config.PurchaseExecution{
			ExecutionID: execID, Status: "failed", Error: "IAM denied: " + fakeAWSDenied,
		}, "", "")
		assertNoProviderText(t, "status_description", row.StatusDescription)
		assertNoProviderText(t, "ops_hint", row.OpsHint)
		assert.Equal(t, "Grant the deploy role missing IAM permission, then retry", row.OpsHint,
			"the hint still resolves from the raw text")
	})

	t.Run("partially completed and audit-gap rows", func(t *testing.T) {
		for _, status := range []string{"partially_completed", "completed"} {
			row := executionToHistoryRow(config.PurchaseExecution{
				ExecutionID: execID, Status: status, Error: "t4g.nano: " + fakeAWSDenied,
			}, "", "")
			assert.NotEmpty(t, row.StatusDescription, status)
			assertNoProviderText(t, status+" status_description", row.StatusDescription)
		}
	})

	t.Run("purchase details", func(t *testing.T) {
		exec := &config.PurchaseExecution{ExecutionID: execID, Status: "failed", Error: fakeAWSDenied,
			Recommendations: []config.RecommendationRecord{rec}}
		resp := buildPurchaseDetailsResponse(exec, "plan")
		body, err := json.Marshal(resp)
		require.NoError(t, err)
		assertNoProviderText(t, "details body", string(body))
		assert.Equal(t, fakeAWSDenied, exec.Error, "stored row is not mutated")
		assert.Equal(t, fakeAWSDenied, exec.Recommendations[0].Error, "stored rec is not mutated")
	})

	t.Run("retry 409 failure_reason", func(t *testing.T) {
		creator := retryCallerID
		failed := &config.PurchaseExecution{ExecutionID: retryExecID, Status: "failed",
			Error: "AssumeRole error: IAM denied: " + fakeAWSDenied, CreatedByUserID: &creator}
		handler, _, _ := buildSessionRetryHandler(t, failed, &Session{UserID: retryCallerID}, false, true)
		_, err := handler.retryPurchase(t.Context(), sessionRetryReq(), retryExecID)
		ce, ok := IsClientError(err)
		require.True(t, ok)
		assert.Equal(t, 409, ce.code)
		body, mErr := json.Marshal(ce.Details())
		require.NoError(t, mErr)
		assertNoProviderText(t, "retry details", string(body)+ce.Error())
		assert.NotEmpty(t, ce.Details()["failure_reason"])
	})
}

func TestExecuteFailureError_PartialAndAuditLossHideProviderText(t *testing.T) {
	for _, status := range []string{"partially_completed", "failed"} {
		err := executeFailureError("e1", "executed", &config.PurchaseExecution{Status: status},
			errors.New("some purchases failed (partial success): "+fakeAWSDenied))
		ce, ok := IsClientError(err)
		require.True(t, ok)
		assertNoProviderText(t, status, ce.Error())
	}
	err := executeFailureError("e1", "executed", &config.PurchaseExecution{Status: "failed"},
		errors.Join(config.ErrAuditLoss, errors.New(fakeAWSDenied)))
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, 500, ce.code)
	assertNoProviderText(t, "audit loss", ce.Error())
}

const (
	fakeSubID  = "00000000-0000-0000-0000-000000000000"
	fakeTenant = "11111111-1111-1111-1111-111111111111"
)

func azureHTTPError(t *testing.T, status int, path string) error {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, "https://management.azure.com"+path, nil)
	require.NoError(t, err)
	return runtime.NewResponseError(&http.Response{
		StatusCode: status, Status: http.StatusText(status), Request: req,
		Header: http.Header{"Content-Type": {"text/html"}},
		Body:   io.NopCloser(strings.NewReader("<html>gateway error</html>")),
	})
}

func assertNoAzureText(t *testing.T, where, s string) {
	t.Helper()
	for _, n := range []string{fakeSubID, fakeTenant, "management.azure.com", "gateway", "ERROR CODE"} {
		assert.NotContains(t, s, n, where)
	}
}

func TestCodelessAzureError_NeverReachesUserSurfaces(t *testing.T) {
	for _, status := range []int{400, 409, 412} {
		raw := azureHTTPError(t, status, "/subscriptions/"+fakeSubID+"/providers/Microsoft.BillingBenefits/savingsPlanOrderAliases/x")
		rec := config.RecommendationRecord{Provider: "azure", Service: "savingsplans", Error: raw.Error()}
		ce, ok := IsClientError(executeFailureError("e1", "executed", &config.PurchaseExecution{Status: "failed"}, raw))
		require.True(t, ok)
		assertNoAzureText(t, "live", ce.Error())
		row := executionToHistoryRow(config.PurchaseExecution{Status: "failed", Error: raw.Error(), Recommendations: []config.RecommendationRecord{rec}}, "", "")
		assert.NotEmpty(t, row.StatusDescription)
		assertNoAzureText(t, "history", row.StatusDescription)
		resp, err := json.Marshal(buildPurchaseDetailsResponse(&config.PurchaseExecution{Status: "failed", Error: raw.Error(), Recommendations: []config.RecommendationRecord{rec}}, "plan"))
		require.NoError(t, err)
		assertNoAzureText(t, "details", string(resp))
	}
}

func TestAzureIdentityText_NeverReachesUserSurfaces(t *testing.T) {
	raw := "ClientSecretCredential authentication failed. POST https://login.microsoftonline.com/" + fakeTenant + "/oauth2/v2.0/token\n" +
		"RESPONSE 400: 400 Bad Request\n{\"error_description\":\"AADSTS700016: Application not found. Trace ID: 22222222 Correlation ID: 33333333\"}"
	ce, ok := IsClientError(executeFailureError("e1", "executed", &config.PurchaseExecution{Status: "failed"}, errors.New(raw)))
	require.True(t, ok)
	for _, s := range []string{ce.Error(), executionToHistoryRow(config.PurchaseExecution{Status: "failed", Error: raw}, "", "").StatusDescription} {
		for _, n := range []string{fakeTenant, "login.microsoftonline.com", "AADSTS", "Trace ID", "Correlation ID"} {
			assert.NotContains(t, s, n)
		}
	}
}
