package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil/lambdatimeout"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #1529. Stub Azure clients, or the real armreservations Return client
// pointed at a local TLS server, and a mock store; nothing here calls Azure.

// assertUncertainReturn checks the uncertain-outcome answer: a 500 telling the
// user to check the Azure portal, the in-flight flag kept, and nothing
// recorded or revoked.
func assertUncertainReturn(t *testing.T, mockStore *MockConfigStore, err error) {
	t.Helper()
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "want the uncertain-outcome ClientError, got %v", err)
	assert.Equal(t, 500, ce.code)
	assert.Equal(t, errRevokeReturnUncertain, ce.message)
	mockStore.AssertCalled(t, "FlipPurchaseRevocationInFlight")
	mockStore.AssertNotCalled(t, "ClearRevocationInFlight")
	mockStore.AssertNotCalled(t, "MarkRevocationReturnPosted")
	mockStore.AssertNotCalled(t, "MarkPurchaseRevoked")
}

// A Return that fails without a definite answer may have been accepted by
// Azure, so the flag that stops a second Return must stay set. Before #1529
// every one of these cleared the flag and answered a flat failure, so a
// refunded purchase could read as active and failed.
func TestCallAzureReturn_AmbiguousFailure_KeepsFlagAndAnswersUncertain(t *testing.T) {
	t.Parallel()
	cases := map[string]error{
		"500":                     &azcore.ResponseError{StatusCode: 500, ErrorCode: "InternalServerError"},
		"502":                     &azcore.ResponseError{StatusCode: 502},
		"503":                     &azcore.ResponseError{StatusCode: 503},
		"504":                     &azcore.ResponseError{StatusCode: 504, ErrorCode: "GatewayTimeout"},
		"408 request timeout":     &azcore.ResponseError{StatusCode: 408},
		"unexpected 200":          &azcore.ResponseError{StatusCode: 200},
		"Return deadline":         context.DeadlineExceeded,
		"Return deadline wrapped": &url.Error{Op: "Post", URL: "https://management.azure.com/x", Err: context.DeadlineExceeded},
		"transport error":         errors.New("dial tcp: connection reset by peer"),
		"response cut off":        fmt.Errorf("reading response body: %w", io.ErrUnexpectedEOF),
	}
	for name, returnErr := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mockStore := new(MockConfigStore)
			calcClient := &stubCalcRefundClientWithAmount{amount: 10.0, currency: "USD", sessID: "s-uncertain"}
			returnClient := &stubReturnClient{err: returnErr}
			h := &Handler{config: mockStore}

			_, err := h.callAzureReturn(context.Background(), calcClient, returnClient, armReservationRecord(), "order-abc", "res-xyz", confirmedQuote(10, "USD"))
			assertUncertainReturn(t, mockStore, err)
			assert.Equal(t, 1, returnClient.calls, "exactly one Return")
		})
	}
}

// A definite refusal (Azure answered the only request with a 4xx) proves no
// refund was issued, so the flag is cleared and the purchase can be revoked
// again, as before #1529.
func TestCallAzureReturn_DefiniteRefusal_ClearsFlag(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		err  error
		code int
	}{
		"400":         {&azcore.ResponseError{StatusCode: 400, ErrorCode: "InvalidReservationID"}, 400},
		"403":         {&azcore.ResponseError{StatusCode: 403, ErrorCode: "AuthorizationFailed"}, 400},
		"409":         {&azcore.ResponseError{StatusCode: 409, ErrorCode: "ReservationAlreadyReturned"}, 400},
		"window edge": {&azcore.ResponseError{StatusCode: 400, ErrorCode: "RefundPolicyViolated"}, 422},
		"401":         {&azcore.ResponseError{StatusCode: 401, ErrorCode: "InvalidAuthenticationToken"}, 0},
		"429":         {&azcore.ResponseError{StatusCode: 429, ErrorCode: "TooManyRequests"}, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mockStore := new(MockConfigStore)
			t.Cleanup(func() { mockStore.AssertExpectations(t) })
			r := armReservationRecord()
			mockStore.On("ClearRevocationInFlight", mock.Anything, r.PurchaseID).Return(nil).Once()
			calcClient := &stubCalcRefundClientWithAmount{amount: 10.0, currency: "USD", sessID: "s-refused"}
			h := &Handler{config: mockStore}

			_, err := h.callAzureReturn(context.Background(), calcClient, &stubReturnClient{err: tc.err}, r, "order-abc", "res-xyz", confirmedQuote(10, "USD"))
			require.Error(t, err)
			ce, isClient := IsClientError(err)
			if tc.code == 0 {
				assert.False(t, isClient, "a refusal without a mapped status stays a plain 500, got %v", err)
			} else {
				require.True(t, isClient, "got %v", err)
				assert.Equal(t, tc.code, ce.code)
			}
			assert.NotEqual(t, errRevokeReturnUncertain, err.Error(), "a definite refusal is not uncertain")
			mockStore.AssertNotCalled(t, "MarkRevocationReturnPosted")
			mockStore.AssertNotCalled(t, "MarkPurchaseRevoked")
		})
	}
}

func TestIsAzureDefiniteRefusal_AttemptsAndStatus(t *testing.T) {
	t.Parallel()
	refused := &azcore.ResponseError{StatusCode: 409}
	assert.True(t, isAzureDefiniteRefusal(refused, 0), "a client without the counting policy is judged on the status")
	assert.True(t, isAzureDefiniteRefusal(refused, 1))
	assert.True(t, isAzureDefiniteRefusal(fmt.Errorf("wrapped: %w", refused), 1))
	assert.False(t, isAzureDefiniteRefusal(refused, 2), "a 4xx after a retry may answer a retry of an accepted try")
	assert.True(t, isAzureDefiniteRefusal(&azcore.ResponseError{StatusCode: 499}, 1))
	assert.False(t, isAzureDefiniteRefusal(&azcore.ResponseError{StatusCode: 500}, 1))
	assert.False(t, isAzureDefiniteRefusal(&azcore.ResponseError{StatusCode: 399}, 1))
	assert.False(t, isAzureDefiniteRefusal(context.Canceled, 1))
}

// fakeTokenCredential satisfies the bearer-token policy without Entra ID.
type fakeTokenCredential struct{}

func (fakeTokenCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "fake-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// returnServer stands in for ARM's Return endpoint. It answers the n-th
// request with answers[n] (the last one repeats) and counts requests.
type returnServer struct {
	*httptest.Server
	requests atomic.Int32
}

func newReturnServer(t *testing.T, answers ...func(http.ResponseWriter)) *returnServer {
	t.Helper()
	s := &returnServer{}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		n := int(s.requests.Add(1)) - 1
		answers[min(n, len(answers)-1)](w)
	}))
	t.Cleanup(s.Close)
	return s
}

// returnClientFor builds the production Return client (the default factory's,
// with returnAttemptPolicy) against srv, with millisecond retry delays.
func returnClientFor(t *testing.T, srv *returnServer) azureReturnClient {
	t.Helper()
	opts := &arm.ClientOptions{
		ClientOptions: policy.ClientOptions{
			Cloud: cloud.Configuration{Services: map[cloud.ServiceName]cloud.ServiceConfiguration{
				cloud.ResourceManager: {Endpoint: srv.URL, Audience: "https://management.core.windows.net/"},
			}},
			Retry:     policy.RetryOptions{RetryDelay: time.Millisecond, MaxRetryDelay: 5 * time.Millisecond},
			Transport: srv.Client(),
		},
		DisableRPRegistration: true,
	}
	client, err := defaultAzureRevokeFactory(opts).newReturnClient(fakeTokenCredential{})
	require.NoError(t, err)
	return client
}

func azureError(status int, code string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-ms-error-code", code)
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":"stub"}}`, code)
	}
}

// truncatedAccepted answers 202 but closes the connection partway through the
// body, so the client cannot read Azure's answer to a Return Azure accepted.
func truncatedAccepted(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", "500")
	w.WriteHeader(http.StatusAccepted)
	_, _ = io.WriteString(w, `{"id":"`)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
		}
	}
}

// The real Return client retries a 5xx, and the retry can meet Azure's
// refusal of a Return it already took. That final 4xx is not a definite
// refusal: the flag must stay. Nor does a 202 whose body was cut off prove
// anything. Before #1529 the flag was cleared in every case.
func TestCallAzureReturn_RealClient_AmbiguousKeepsFlag(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		answers  []func(http.ResponseWriter)
		requests int32
	}{
		"503 then 409":              {[]func(http.ResponseWriter){azureError(503, "ServiceUnavailable"), azureError(409, "ReservationAlreadyReturned")}, 2},
		"504 then 400 RefundPol.":   {[]func(http.ResponseWriter){azureError(504, "GatewayTimeout"), azureError(400, "RefundPolicyViolated")}, 2},
		"503 on every try":          {[]func(http.ResponseWriter){azureError(503, "ServiceUnavailable")}, 4},
		"202 with its body cut off": {[]func(http.ResponseWriter){truncatedAccepted, azureError(400, "ReservationAlreadyReturned")}, 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := newReturnServer(t, tc.answers...)
			mockStore := new(MockConfigStore)
			calcClient := &stubCalcRefundClientWithAmount{amount: 10.0, currency: "USD", sessID: "s-retry"}
			h := &Handler{config: mockStore}

			_, err := h.callAzureReturn(context.Background(), calcClient, returnClientFor(t, srv), armReservationRecord(), "order-abc", "res-xyz", confirmedQuote(10, "USD"))
			assertUncertainReturn(t, mockStore, err)
			assert.Equal(t, tc.requests, srv.requests.Load(), "requests the client sent")
		})
	}
}

// A 4xx on the only request the real client sent is a definite refusal: the
// counting policy sees one send and the flag is cleared.
func TestCallAzureReturn_RealClient_Single4xxClearsFlag(t *testing.T) {
	t.Parallel()
	srv := newReturnServer(t, azureError(400, "InvalidReservationID"))
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	r := armReservationRecord()
	mockStore.On("ClearRevocationInFlight", mock.Anything, r.PurchaseID).Return(nil).Once()
	calcClient := &stubCalcRefundClientWithAmount{amount: 10.0, currency: "USD", sessID: "s-single"}
	h := &Handler{config: mockStore}

	_, err := h.callAzureReturn(context.Background(), calcClient, returnClientFor(t, srv), r, "order-abc", "res-xyz", confirmedQuote(10, "USD"))
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, 400, ce.code)
	assert.Equal(t, int32(1), srv.requests.Load())
	mockStore.AssertNotCalled(t, "MarkPurchaseRevoked")
}

// The real client's 202 still revokes; the counting policy changes nothing on
// the success path.
func TestCallAzureReturn_RealClient_AcceptedRevokes(t *testing.T) {
	t.Parallel()
	srv := newReturnServer(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{}`)
	})
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	r := armReservationRecord()
	mockStore.On("MarkRevocationReturnPosted", mock.Anything, r.PurchaseID).Return(nil).Once()
	mockStore.On("MarkPurchaseRevoked", mock.Anything, r.PurchaseID, mock.AnythingOfType("time.Time"),
		"direct-api", "", mock.Anything, mock.Anything).Return(nil).Once()
	calcClient := &stubCalcRefundClientWithAmount{amount: 10.0, currency: "USD", sessID: "s-ok"}
	h := &Handler{config: mockStore}

	result, err := h.callAzureReturn(context.Background(), calcClient, returnClientFor(t, srv), r, "order-abc", "res-xyz", confirmedQuote(10, "USD"))
	require.NoError(t, err)
	rr, ok := result.(*revokePurchaseResult)
	require.True(t, ok, "got %T %+v", result, result)
	assert.Equal(t, "revoked", rr.Status)
	assert.Equal(t, int32(1), srv.requests.Load())
}

// The claim threshold is the detached Return plus one post-Return write budget
// plus the response margin, and it must leave every shipped Lambda timeout
// (at least those budgets plus lambdatimeout.Margin) time for the work before
// the claim, or every Lambda revoke would be refused.
func TestRevokeClaimMinRemaining_Budget(t *testing.T) {
	t.Parallel()
	assert.Equal(t, revokeReturnTimeout+revokePersistTimeout+revokeResponseMargin, revokeClaimMinRemaining)
	assert.Equal(t, 3*time.Minute+35*time.Second, revokeClaimMinRemaining)
	assert.Less(t, revokeClaimMinRemaining, revokeReturnTimeout+revokePersistTimeout+lambdatimeout.Margin)
}

// On Lambda, a revoke with too little invocation time left must not take the
// claim or post the Return. Before #1529 it did both, and Lambda could end the
// invocation mid-Return.
func TestCallAzureReturn_LambdaTooLittleTime_RefusesBeforeClaim(t *testing.T) {
	t.Parallel()
	for name, left := range map[string]time.Duration{
		"one minute":            time.Minute,
		"just under the needed": revokeClaimMinRemaining - time.Second,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), left)
			defer cancel()
			mockStore := new(MockConfigStore)
			// Registered so a regression fails the assertions below instead
			// of panicking the test binary on an unexpected call.
			mockStore.On("MarkPurchaseRevoked", mock.Anything, mock.Anything, mock.Anything,
				mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
			calcClient := &stubCalcRefundClientWithAmount{amount: 10.0, currency: "USD", sessID: "s-late"}
			returnClient := &stubReturnClient{}
			h := &Handler{config: mockStore, isLambda: true}

			result, err := h.callAzureReturn(ctx, calcClient, returnClient, armReservationRecord(), "order-abc", "res-xyz", confirmedQuote(10, "USD"))
			require.Error(t, err)
			assert.Nil(t, result)
			ce, ok := IsClientError(err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, http.StatusServiceUnavailable, ce.code)
			assert.Equal(t, errRevokeTooLittleTime, ce.message)
			assert.Zero(t, returnClient.calls, "no Return may be posted")
			mockStore.AssertNotCalled(t, "FlipPurchaseRevocationInFlight")
			mockStore.AssertNotCalled(t, "ClearRevocationInFlight")
			mockStore.AssertNotCalled(t, "MarkRevocationReturnPosted")
			mockStore.AssertNotCalled(t, "MarkPurchaseRevoked")
		})
	}
}

// The check must not refuse a revoke that has the time, nor apply off Lambda,
// where the process outlives the request's 30s deadline and the detached
// Return finishes anyway.
func TestCallAzureReturn_EnoughTimeOrNotLambda_Proceeds(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		left     time.Duration // 0: no deadline
		isLambda bool
	}{
		"lambda with time to spare":     {revokeClaimMinRemaining + 30*time.Second, true},
		"lambda without a deadline":     {0, true},
		"http server's 30s deadline":    {20 * time.Second, false},
		"non-lambda with little time":   {time.Second, false},
		"non-lambda without a deadline": {0, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			if tc.left > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.left)
				defer cancel()
			}
			mockStore := new(MockConfigStore)
			r := armReservationRecord()
			mockStore.On("MarkPurchaseRevoked", mock.Anything, r.PurchaseID, mock.AnythingOfType("time.Time"),
				"direct-api", "", mock.Anything, mock.Anything).Return(nil).Once()
			calcClient := &stubCalcRefundClientWithAmount{amount: 10.0, currency: "USD", sessID: "s-time"}
			returnClient := &stubReturnClient{}
			h := &Handler{config: mockStore, isLambda: tc.isLambda}

			result, err := h.callAzureReturn(ctx, calcClient, returnClient, r, "order-abc", "res-xyz", confirmedQuote(10, "USD"))
			require.NoError(t, err)
			rr, ok := result.(*revokePurchaseResult)
			require.True(t, ok, "got %T %+v", result, result)
			assert.Equal(t, "revoked", rr.Status)
			assert.Equal(t, 1, returnClient.calls)
			mockStore.AssertCalled(t, "FlipPurchaseRevocationInFlight")
		})
	}
}
