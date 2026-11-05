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
	sdk := func(svc, status, code string) string {
		return "operation error " + svc + ": X, https response error StatusCode: " + status + ", RequestID: r, api error " + code + ": msg"
	}
	retried := func(svc, status, code string) string {
		return "operation error " + svc + ": X, exceeded maximum number of attempts, 3, https response error StatusCode: " + status + ", RequestID: r, api error " + code + ": msg"
	}
	ec2 := func(status, code string) string { return "failed to purchase EC2 RI: " + sdk("EC2", status, code) }
	storedEC2 := "m5.large: purchase failed: failed to purchase EC2 RI: operation error EC2: PurchaseReservedInstancesOffering, https response error StatusCode: 403, RequestID: 11111111-2222-3333-4444-555555555555, api error UnauthorizedOperation: You are not authorized to perform this operation (account 123456789012)"
	storedRedshift := "ra3.xlplus: purchase failed: failed to purchase Redshift Reserved Node: " + sdk("Redshift", "400", "InvalidParameterValue")
	azure429 := "PUT https://management.azure.com/x\nRESPONSE 429: 429 Too Many\nERROR CODE: TooManyRequests"
	gcp := "commitment creation failed: googleapi: Error 403: Rate Limit Exceeded, rateLimitExceeded"
	type tc struct {
		name string
		recs []string
		err  string
		want bool
	}
	retrying := []struct{ head, svc string }{
		{"failed to purchase RDS RI: ", "RDS"},
		{"failed to purchase Reserved Cache Node: ", "ElastiCache"},
		{"failed to purchase OpenSearch RI: ", "OpenSearch"},
		{"failed to purchase MemoryDB Reserved Nodes: ", "MemoryDB"},
		{"failed to purchase Savings Plan: ", "savingsplans"},
	}
	base := []tc{
		{"ec2 403 denied", []string{"aws"}, ec2("403", "UnauthorizedOperation"), true},
		{"empty provider is not aws", []string{""}, ec2("403", "AccessDenied"), false},
		{"empty provider beside aws", []string{"aws", ""}, ec2("403", "AccessDenied"), false},
		{"ec2 stored shape", []string{"aws"}, storedEC2, true},
		{"redshift stored shape", []string{"aws"}, storedRedshift, true},
		{"stored shape without resource type", []string{"aws"}, "purchase failed: " + ec2("403", "AccessDenied"), true},
		{"stored shape without purchase failed", []string{"aws"}, "m5.large: " + ec2("403", "AccessDenied"), true},
		{"stored shape, two recs", []string{"aws", "aws"}, storedEC2 + "; " + storedRedshift, true},
		{"stored shape, partial prefix", []string{"aws"}, partialRunPrefix + storedEC2, true},
		{"bare sdk error, no library head", []string{"aws"}, sdk("EC2", "403", "AccessDenied"), false},
		{"resource token, no library head", []string{"aws"}, "m5.large: " + sdk("EC2", "403", "AccessDenied"), false},
		{"unknown head before operation error", []string{"aws"}, "m5.large: purchase failed: something else: " + sdk("EC2", "403", "AccessDenied"), false},
		{"unknown library head", []string{"aws"}, "m5.large: purchase failed: failed to purchase GPU RI: " + sdk("EC2", "403", "AccessDenied"), false},
		{"head in another case", []string{"aws"}, "Failed to purchase EC2 RI: " + sdk("EC2", "403", "AccessDenied"), false},
		{"head after another head", []string{"aws"}, "purchase failed: purchase failed: " + ec2("403", "AccessDenied"), false},
		{"stored shape 5xx", []string{"aws"}, strings.Replace(storedEC2, "StatusCode: 403", "StatusCode: 503", 1), false},
		{"stored shape outcome unknown", []string{"aws"}, "m5.large: purchase failed: purchase outcome unknown: " + ec2("403", "AccessDenied"), false},
		{"status outside the response error", []string{"aws"}, "failed to purchase EC2 RI: operation error EC2: X, https request send failed: StatusCode: 403, api error AccessDenied: msg", false},
		{"ec2 throttle 400", []string{"aws"}, ec2("400", "Throttling"), true},
		{"ec2 invalid offering", []string{"aws"}, ec2("400", "InvalidParameterValue"), true},
		{"per-rec segments", []string{"aws", "aws"}, "a: " + ec2("403", "AccessDenied") + "; b: " + ec2("400", "Throttling"), true},
		{"ec2 retry exhausted", []string{"aws"}, "failed to purchase EC2 RI: " + retried("EC2", "400", "Throttling"), false},
		{"redshift retry exhausted", []string{"aws"}, "failed to purchase Redshift Reserved Node: " + retried("Redshift", "400", "Throttling"), false},
		{"retry quota exceeded", []string{"aws"}, "failed to purchase EC2 RI: operation error EC2: X, failed to get rate limit token, retry quota exceeded, https response error StatusCode: 400, api error Throttling: msg", false},
		{"5xx", []string{"aws"}, ec2("503", "AccessDenied"), false},
		{"no code", []string{"aws"}, "failed to purchase EC2 RI: operation error EC2: X, https response error StatusCode: 403", false},
		{"unlisted code", []string{"aws"}, ec2("400", "SomethingOdd"), false},
		{"azure rec", []string{"azure"}, azure429, false},
		{"azure text on aws rec", []string{"aws"}, azure429, false},
		{"gcp rec", []string{"gcp"}, gcp, false},
		{"mixed recs", []string{"aws", "gcp"}, ec2("403", "AccessDenied"), false},
		{"server note", []string{"aws"}, "history_write_failed: commitment c purchased but its history record failed to save", false},
		{"unknown outcome wrapper", []string{"aws"}, "purchase outcome unknown: the request may have been sent; reconcile before retrying: " + ec2("400", "Throttling"), false},
		{"partial prefix", []string{"aws"}, partialRunPrefix + "a: " + ec2("403", "AccessDenied"), true},
		{"one unknown segment", []string{"aws", "aws"}, "a: " + ec2("403", "AccessDenied") + "; b: purchase outcome unknown", false},
		{"semicolon in message", []string{"aws"}, ec2("400", "InvalidParameterValue") + "; reserved elsewhere", false},
		{"no recs", nil, ec2("403", "AccessDenied"), false},
		{"empty", []string{"aws"}, "", false},
	}
	cases := make([]tc, 0, len(base)+2*len(retrying))
	cases = append(cases, base...)
	// Token-carrying services keep the SDK retries, so neither a single-attempt
	// looking 4xx nor a retry-exhausted one proves nothing was bought.
	for _, r := range retrying {
		cases = append(cases,
			tc{r.svc + " 4xx keeps the hint", []string{"aws"}, "x.large: purchase failed: " + r.head + sdk(r.svc, "403", "AccessDenied"), false},
			tc{r.svc + " retried 4xx keeps the hint", []string{"aws"}, "x.large: purchase failed: " + r.head + retried(r.svc, "400", "ThrottlingException"), false},
		)
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
