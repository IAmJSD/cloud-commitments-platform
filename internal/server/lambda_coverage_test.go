package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/api"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/mocks"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/mock"
)

func TestIsTextContentType(t *testing.T) {
	tests := []struct {
		ct       string
		expected bool
	}{
		{"text/html", true},
		{"text/plain", true},
		{"text/css", true},
		{"text/javascript", true},
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"application/javascript", true},
		{"application/xml", true},
		{"image/svg+xml", true},
		{"image/png", false},
		{"image/jpeg", false},
		{"application/octet-stream", false},
		{"font/woff2", false},
	}
	for _, tt := range tests {
		t.Run(tt.ct, func(t *testing.T) {
			testutil.AssertEqual(t, tt.expected, isTextContentType(tt.ct))
		})
	}
}

func TestServeLambdaStatic_Found(t *testing.T) {
	dir := makeStaticDir(t, map[string]string{
		"index.html": "<html>hello</html>",
		"app.js":     "var x=1;",
	})

	app := &Application{staticDir: dir}

	// Text file — body should be plain string
	resp, err := app.serveLambdaStatic("/index.html")
	testutil.AssertNoError(t, err)
	testutil.AssertEqual(t, 200, resp.StatusCode)
	testutil.AssertEqual(t, false, resp.IsBase64Encoded)
	testutil.AssertContains(t, resp.Body, "<html>")
}

// TestLambdaSecurityHeaders_IncludesCSP locks in that Lambda HTML responses
// carry a Content-Security-Policy header with frame-ancestors 'none'.
// Without it, the meta-tag CSP in index.html can't enforce frame-ancestors
// (browsers ignore that directive in <meta>), leaving the Lambda deploy
// unprotected against clickjacking. See issues/8.
func TestLambdaSecurityHeaders_IncludesCSP(t *testing.T) {
	h := lambdaSecurityHeaders()
	csp, ok := h["Content-Security-Policy"]
	testutil.AssertTrue(t, ok, "lambdaSecurityHeaders must set Content-Security-Policy")
	testutil.AssertContains(t, csp, "frame-ancestors 'none'")
	testutil.AssertContains(t, csp, "default-src 'self'")
}

func TestServeLambdaStatic_BinaryFile(t *testing.T) {
	dir := makeStaticDir(t, map[string]string{
		"index.html": "<html/>",
		"img.png":    "\x89PNG\x0d\x0a",
	})

	app := &Application{staticDir: dir}

	resp, err := app.serveLambdaStatic("/img.png")
	testutil.AssertNoError(t, err)
	testutil.AssertEqual(t, 200, resp.StatusCode)
	testutil.AssertEqual(t, true, resp.IsBase64Encoded)
}

func TestServeLambdaStatic_NotFound(t *testing.T) {
	dir := makeStaticDir(t, map[string]string{"index.html": "<html/>"})

	app := &Application{staticDir: dir}

	resp, err := app.serveLambdaStatic("/missing.png")
	testutil.AssertNoError(t, err)
	testutil.AssertEqual(t, 404, resp.StatusCode)
}

func TestHandleLambdaHTTPEvent_StaticPath(t *testing.T) {
	dir := makeStaticDir(t, map[string]string{"index.html": "<html>spa</html>"})

	app := &Application{
		API:       api.NewHandler(api.HandlerConfig{}),
		staticDir: dir,
	}

	rawEvent := json.RawMessage(`{
		"requestContext": {"http": {"method": "GET"}},
		"rawPath": "/dashboard",
		"headers": {}
	}`)

	ctx := testutil.TestContext(t)
	resp, err := app.handleLambdaHTTPEvent(ctx, rawEvent)
	testutil.AssertNoError(t, err)
	testutil.AssertEqual(t, 200, resp.StatusCode)
}

// TestHandleLambdaHTTPEvent_DecodesBase64FormBody is a regression test for the
// Lambda Function URL body-decode gap (follow-up to #889): AWS delivers POST
// bodies base64-encoded whenever the Content-Type isn't recognized as plain
// text -- the inbound mirror of the isTextContentType decision this file
// already makes for outbound responses. Before the fix, handleLambdaHTTPEvent
// passed the raw base64 blob straight through to the API router, so
// resolveApprovalToken (internal/api/router.go) could never recover the
// "token=..." pair from the one-click revoke confirmation form's
// x-www-form-urlencoded POST body: every revoke via the email link 401'd in
// Lambda Function URL mode (the primary deploy mode), even though the same
// flow worked under the HTTP/Fargate adapter (which never base64-encodes the
// body it builds).
//
// This drives the real failing scenario end-to-end: a base64-encoded,
// form-urlencoded POST body hitting POST /api/purchases/revoke/{execID}.
// The session lacks cancel permission but is the account's contact email.
// When the token fails to resolve, revokeViaEmailToken answers the 404 a
// missing execution gets (issue #597). Once the token has been parsed out of
// the decoded body, it falls through to the token branch, passes the
// contact_email gate, and answers 403 "invalid revocation token" because the
// token does not match. Only the second answer is reachable when the token
// resolves, so asserting it proves the fix. (A request with no session never
// reaches either: it gets 401 before the lookup, issue #435.)
func TestHandleLambdaHTTPEvent_DecodesBase64FormBody(t *testing.T) {
	execID := "11111111-1111-1111-1111-111111111111"
	accountID := "acct-1"
	exec := &config.PurchaseExecution{
		ExecutionID:     execID,
		Status:          "completed",
		ApprovalToken:   config.HashApprovalToken("a-different-token"),
		Recommendations: []config.RecommendationRecord{{ID: "r1", CloudAccountID: &accountID}},
	}

	mockStore := new(mocks.MockConfigStore)
	mockStore.On("GetExecutionByID", mock.Anything, execID).Return(exec, nil)
	mockStore.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{}, nil)
	mockStore.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
		return &config.CloudAccount{ID: id, ContactEmail: "user@example.com"}, nil
	}

	app := &Application{
		API: api.NewHandler(api.HandlerConfig{ConfigStore: mockStore, AuthService: sessionOnlyAuth{}}),
	}

	encodedBody := base64.StdEncoding.EncodeToString([]byte("token=body-token"))
	request := events.LambdaFunctionURLRequest{
		RawPath: "/api/purchases/revoke/" + execID,
		Headers: map[string]string{
			"content-type":  "application/x-www-form-urlencoded",
			"authorization": "Bearer sess-tok",
			"x-csrf-token":  "csrf-ok",
		},
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{
				Method: "POST",
				Path:   "/api/purchases/revoke/" + execID,
			},
		},
		Body:            encodedBody,
		IsBase64Encoded: true,
	}
	rawEvent, err := json.Marshal(request)
	testutil.AssertNoError(t, err)

	ctx := testutil.TestContext(t)
	resp, err := app.handleLambdaHTTPEvent(ctx, rawEvent)
	testutil.AssertNoError(t, err)
	testutil.AssertEqual(t, 403, resp.StatusCode)
	testutil.AssertContains(t, resp.Body, "invalid revocation token")
	testutil.AssertTrue(t, !strings.Contains(resp.Body, "execution not found"),
		"token must have been resolved from the decoded body, not left empty")

	mockStore.AssertExpectations(t)
}

func TestHandleLambdaEvent_UnknownEventRouteToScheduled(t *testing.T) {
	// "unknown" event type routes to handleLambdaScheduledEvent, which
	// needs a parseable action. Empty object will fail ParseScheduledEvent.
	app := &Application{
		API: api.NewHandler(api.HandlerConfig{}),
	}

	rawEvent := json.RawMessage(`{"random_key": "random_value"}`)
	ctx := context.Background()
	_, err := app.HandleLambdaEvent(ctx, rawEvent)
	// Unknown action → error from ParseScheduledEvent
	testutil.AssertError(t, err)
}

// sessionOnlyAuth accepts the bearer "sess-tok" as a signed-in user with no
// purchase permissions; any other method panics via the nil embedded interface.
type sessionOnlyAuth struct{ api.AuthServiceInterface }

func (sessionOnlyAuth) ValidateSession(_ context.Context, token string) (*api.Session, error) {
	if token != "sess-tok" {
		return nil, errors.New("invalid session")
	}
	return &api.Session{UserID: "user-1", Email: "user@example.com"}, nil
}

func (sessionOnlyAuth) HasPermissionAPI(context.Context, string, string, string) (bool, error) {
	return false, nil
}

func (sessionOnlyAuth) GetAllowedAccountsAPI(context.Context, string) ([]string, error) {
	return nil, nil
}

// ValidateCSRFToken accepts "csrf-ok" for "sess-tok": the email-link routes
// validate CSRF before the execution lookup (issue #597).
func (sessionOnlyAuth) ValidateCSRFToken(_ context.Context, sessionToken, csrfToken string) error {
	if sessionToken != "sess-tok" || csrfToken != "csrf-ok" {
		return errors.New("invalid CSRF token")
	}
	return nil
}

// TestSecurityHeaders_HSTSMatchesAcrossModes pins every response path to the
// same Strict-Transport-Security value (issue #14: Lambda mode had dropped
// "preload" while HTTP mode sent it).
func TestSecurityHeaders_HSTSMatchesAcrossModes(t *testing.T) {
	testutil.AssertContains(t, api.HSTSHeaderValue, "preload")

	testutil.AssertEqual(t, api.HSTSHeaderValue, lambdaSecurityHeaders()["Strict-Transport-Security"])

	rec := httptest.NewRecorder()
	securityHeaders(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	testutil.AssertEqual(t, api.HSTSHeaderValue, rec.Header().Get("Strict-Transport-Security"))

	rec = httptest.NewRecorder()
	setHealthResponseHeaders(rec, "")
	testutil.AssertEqual(t, api.HSTSHeaderValue, rec.Header().Get("Strict-Transport-Security"))
}
