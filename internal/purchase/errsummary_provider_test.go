package purchase

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/googleapi"
)

const (
	fakeSub    = "00000000-0000-0000-0000-000000000000"
	fakeTenant = "11111111-1111-1111-1111-111111111111"
)

var providerDetailNeedles = []string{fakeSub, fakeTenant, "management.azure.com", "login.microsoftonline.com",
	"Trace ID", "Correlation ID", "AADSTS", "123456789012", "gateway"}

func assertNoProviderDetail(t *testing.T, where, s string) {
	t.Helper()
	for _, n := range providerDetailNeedles {
		assert.NotContains(t, s, n, where)
	}
}

func azureResponseError(t *testing.T, status int, path, contentType, body string) error {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, "https://management.azure.com"+path, nil)
	require.NoError(t, err)
	resp := &http.Response{
		StatusCode: status, Status: http.StatusText(status), Request: req,
		Header: http.Header{"Content-Type": {contentType}},
		Body:   io.NopCloser(strings.NewReader(body)),
	}
	return runtime.NewResponseError(resp)
}

type fakeTransport struct{ status int }

func (f fakeTransport) Do(r *http.Request) (*http.Response, error) {
	body := `{"error":"unauthorized_client","error_description":"AADSTS700016: Application not found in tenant ` + fakeTenant + `. Trace ID: 22222222 Correlation ID: 33333333"}`
	return &http.Response{StatusCode: f.status, Status: "400 Bad Request", Request: r,
		Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func aadError(t *testing.T) error {
	t.Helper()
	cred, err := azidentity.NewClientSecretCredential(fakeTenant, fakeSub, "fake-secret", &azidentity.ClientSecretCredentialOptions{
		ClientOptions:            azcore.ClientOptions{Transport: fakeTransport{status: 400}},
		DisableInstanceDiscovery: true,
	})
	require.NoError(t, err)
	_, err = cred.GetToken(context.Background(), policy.TokenRequestOptions{Scopes: []string{"https://management.azure.com/.default"}})
	require.Error(t, err)
	return err
}

func TestAzureCodelessErrors_StoredAndLiveAreSanitized(t *testing.T) {
	for _, status := range []int{400, 409, 412} {
		err := azureResponseError(t, status, "/subscriptions/"+fakeSub+"/providers/Microsoft.Capacity/reservationOrders", "text/html", "<html>gateway error</html>")
		require.Contains(t, err.Error(), "ERROR CODE UNAVAILABLE")
		stored := SanitizeStoredError(err.Error())
		assertNoProviderDetail(t, "stored", stored)
		assert.NotEmpty(t, stored)
		_, live := SummarizeProviderError(err)
		assertNoProviderDetail(t, "live", live)
		_, liveStored := SummarizeProviderError(err)
		assert.Equal(t, ErrCategoryMessage(ErrCategoryUnknown), liveStored)
	}
}

func TestAzureIdentityFailure_StoredAndLiveAreSanitized(t *testing.T) {
	err := aadError(t)
	var afe *azidentity.AuthenticationFailedError
	require.ErrorAs(t, err, &afe)
	require.Contains(t, err.Error(), "AADSTS700016")
	assertNoProviderDetail(t, "stored", SanitizeStoredError(err.Error()))
	cat, msg := SummarizeProviderError(err)
	assert.Equal(t, ErrCategoryAccessDenied, cat)
	assertNoProviderDetail(t, "live", msg)
}

// A 500 whose URL names Microsoft.Capacity is an unknown outcome, never "not available".
func TestAzure500OnCapacityURL_IsUnknown(t *testing.T) {
	err := azureResponseError(t, 500, "/providers/Microsoft.Capacity/reservationOrders/x", "application/json", `{"error":{"code":"InternalServerError","message":"boom"}}`)
	cat, _ := SummarizeProviderError(err)
	assert.Equal(t, ErrCategoryUnknown, cat)
	assert.Equal(t, ErrCategoryUnknown, classifyStoredText(err.Error()))
}

func TestClassifyStoredText_IgnoresWordsOutsideCodeFields(t *testing.T) {
	for _, s := range []string{
		"PUT https://management.azure.com/providers/Microsoft.Capacity/x\nRESPONSE 500: 500 Internal\nERROR CODE: InternalServerError",
		"something about quota and forbidden and notfound",
		"operation error EC2: X, https response error StatusCode: 500, api error InternalError: capacity forbidden",
	} {
		assert.Equal(t, ErrCategoryUnknown, classifyStoredText(s), s)
	}
}

func TestGoogleAPIErrors_ClassifyByCodeAndReason(t *testing.T) {
	rate := &googleapi.Error{Code: 403, Message: "Rate Limit Exceeded", Errors: []googleapi.ErrorItem{{Reason: "rateLimitExceeded", Message: "Rate Limit Exceeded"}}}
	cat, _ := SummarizeProviderError(rate)
	assert.Equal(t, ErrCategoryLimit, cat)
	assert.Equal(t, ErrCategoryLimit, classifyStoredText(rate.Error()))
	nf := &googleapi.Error{Code: 404, Message: "operations/op-1 was not found", Errors: []googleapi.ErrorItem{{Reason: "notFound", Message: "operations/op-1 was not found"}}}
	assert.Equal(t, ErrCategoryUnavailable, classifyStoredText(nf.Error()))
	multi := &googleapi.Error{Code: 403, Message: "m", Errors: []googleapi.ErrorItem{{Reason: "rateLimitExceeded", Message: "a"}, {Reason: "x", Message: "b"}}}
	assert.Equal(t, ErrCategoryLimit, classifyStoredText(multi.Error()))
}

func TestSanitizeStoredError_WholeRemainderReplaced(t *testing.T) {
	const awsWithSemicolon = "operation error EC2: X, https response error StatusCode: 400, api error InvalidParameterValue: offering rejected; reserved for account 123456789012 in us-east-1"
	for _, in := range []string{
		awsWithSemicolon,
		"t4g.nano: " + awsWithSemicolon,
		"t4g.nano: " + awsWithSemicolon + "; t4g.micro: " + awsWithSemicolon,
		"history_write_failed: commitment c-1 purchased but its history record failed to save; " + awsWithSemicolon,
		"plain provider words with no marker at all",
	} {
		out := SanitizeStoredError(in)
		assert.NotContains(t, out, "123456789012", in)
		assert.NotContains(t, out, "reserved for", in)
		assert.NotEmpty(t, out)
		assert.Equal(t, 1, strings.Count(out, "the provider"), in)
	}
	out := SanitizeStoredError(partialRunPrefix + "t4g.nano: " + awsWithSemicolon)
	assert.True(t, strings.HasPrefix(out, partialRunPrefix))
	assert.NotContains(t, out, "123456789012")
	assert.Equal(t, "", SanitizeStoredError(""))
}

func TestIsDefiniteAWSRefusal(t *testing.T) {
	aws := func(status, code string) string {
		return "operation error EC2: X, https response error StatusCode: " + status + ", RequestID: r, api error " + code + ": msg"
	}
	azure429 := "PUT https://management.azure.com/x\nRESPONSE 429: 429 Too Many\nERROR CODE: TooManyRequests"
	gcp := "commitment creation failed: googleapi: Error 403: Rate Limit Exceeded, rateLimitExceeded"
	cases := []struct {
		name string
		recs []string
		err  string
		want bool
	}{
		{"aws 403 denied", []string{"aws"}, aws("403", "UnauthorizedOperation"), true},
		{"legacy empty provider", []string{""}, aws("403", "AccessDenied"), true},
		{"aws throttle 400", []string{"aws"}, aws("400", "Throttling"), true},
		{"aws invalid offering", []string{"aws"}, aws("400", "InvalidParameterValue"), true},
		{"per-rec segments", []string{"aws", "aws"}, "a: " + aws("403", "AccessDenied") + "; b: " + aws("400", "Throttling"), true},
		{"partial prefix", []string{"aws"}, partialRunPrefix + "a: " + aws("403", "AccessDenied"), true},
		{"5xx", []string{"aws"}, aws("503", "AccessDenied"), false},
		{"no code", []string{"aws"}, "operation error EC2: X, https response error StatusCode: 403", false},
		{"unlisted code", []string{"aws"}, aws("400", "SomethingOdd"), false},
		{"azure rec", []string{"azure"}, azure429, false},
		{"azure text on aws rec", []string{"aws"}, azure429, false},
		{"gcp rec", []string{"gcp"}, gcp, false},
		{"mixed recs", []string{"aws", "gcp"}, aws("403", "AccessDenied"), false},
		{"server note", []string{"aws"}, "history_write_failed: commitment c purchased but its history record failed to save", false},
		{"unknown outcome wrapper", []string{"aws"}, "purchase outcome unknown: the request may have been sent; reconcile before retrying: " + aws("400", "Throttling"), false},
		{"one unknown segment", []string{"aws", "aws"}, "a: " + aws("403", "AccessDenied") + "; b: purchase outcome unknown", false},
		{"semicolon in message", []string{"aws"}, aws("400", "InvalidParameterValue") + "; reserved elsewhere", false},
		{"no recs", nil, aws("403", "AccessDenied"), false},
		{"empty", []string{"aws"}, "", false},
	}
	for _, c := range cases {
		var recs []config.RecommendationRecord
		for _, p := range c.recs {
			recs = append(recs, config.RecommendationRecord{Provider: p})
		}
		assert.Equal(t, c.want, IsDefiniteAWSRefusal(recs, c.err), c.name)
	}
}

func TestClassifyStoredText_ReadsEveryGoogleReason(t *testing.T) {
	multi := &googleapi.Error{Code: 403, Message: "m", Errors: []googleapi.ErrorItem{{Reason: "x", Message: "a"}, {Reason: "rateLimitExceeded", Message: "b"}}}
	assert.Contains(t, multi.Error(), "Reason: rateLimitExceeded")
	assert.Equal(t, ErrCategoryLimit, classifyStoredText(multi.Error()))
	cat, _ := SummarizeProviderError(multi)
	assert.Equal(t, ErrCategoryLimit, cat)
}

// After an accepted Insert the commitment may exist, so a failed wait is never
// a definite category.
func TestPostInsertWaitFailure_IsUnconfirmed(t *testing.T) {
	for _, g := range []*googleapi.Error{
		{Code: 404, Message: "operations/op-1 was not found", Errors: []googleapi.ErrorItem{{Reason: "notFound", Message: "operations/op-1 was not found"}}},
		{Code: 403, Message: "denied", Errors: []googleapi.ErrorItem{{Reason: "forbidden", Message: "denied"}}},
	} {
		err := fmt.Errorf("%w: commitment creation failed: %w", errors.New("retry: permanent error, do not retry"), g)
		cat, msg := SummarizeProviderError(err)
		assert.Equal(t, ErrCategoryUnknown, cat)
		assert.Contains(t, msg, "outcome could not be confirmed")
		assert.Equal(t, ErrCategoryUnknown, classifyStoredText(err.Error()))
		assert.Contains(t, SanitizeStoredError(err.Error()), "outcome could not be confirmed")
		assert.Contains(t, SanitizeStoredError(partialRunPrefix+"x: "+err.Error()), "outcome could not be confirmed")
		assert.NotContains(t, SanitizeStoredError(err.Error()), "not available")
	}
}
