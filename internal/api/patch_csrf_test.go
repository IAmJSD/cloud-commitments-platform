package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// CSRF on PATCH (issue #1101).
//
// requiresCSRFValidation used to allow-list POST, PUT and DELETE, so the two
// session-authed PATCH routes (PATCH /api/plans/{id} and
// PATCH /api/ladder/tranches/{id}) changed state with no CSRF check. The
// predicate now exempts only the safe methods GET, HEAD and OPTIONS, so any
// other verb, including one added later, is checked.

const (
	patchCSRFGood  = "patch-csrf-ok"
	patchCSRFPlan  = "11111111-1111-1111-1111-111111111111"
	patchCSRFOther = "patch-csrf-wrong"
)

// patchCSRFFixture is one PATCH route's handler and a probe that reports
// whether the route's state-changing store call ran.
type patchCSRFFixture struct {
	h       *Handler
	session string
	mutated func() bool
}

type patchCSRFRow struct {
	name  string
	path  string
	body  string
	build func(t *testing.T) patchCSRFFixture
}

var patchCSRFRows = []patchCSRFRow{
	{"plans", "/api/plans/" + patchCSRFPlan, `{"name":"Renamed"}`, patchPlanCSRFFixture},
	{"ladder tranches", "/api/ladder/tranches/" + scopedInAccount,
		`{"expected_revision":0,"scheduled_date":"2030-01-01T00:00:00Z","amount_usd_hr":"1.5"}`, ladderAmendCSRFFixture},
}

// expectCSRF answers ValidateCSRFToken the way the real service does: only
// the issued token passes.
func expectCSRF(m *MockAuthService, session string) {
	m.On("ValidateCSRFToken", mock.Anything, session, patchCSRFGood).Return(nil).Maybe()
	m.On("ValidateCSRFToken", mock.Anything, session, mock.MatchedBy(func(tok string) bool { return tok != patchCSRFGood })).
		Return(errors.New("invalid CSRF token")).Maybe()
}

func patchPlanCSRFFixture(t *testing.T) patchCSRFFixture {
	t.Helper()
	const session = "patch-csrf-admin"
	store := new(MockConfigStore)
	m := new(MockAuthService)
	m.On("ValidateSession", mock.Anything, session).
		Return(&Session{UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Email: "admin@example.com"}, nil)
	m.grantAdmin()
	expectCSRF(m, session)

	store.On("GetPurchasePlan", mock.Anything, patchCSRFPlan).Return(&config.PurchasePlan{
		ID:                     patchCSRFPlan,
		Name:                   "Original Name",
		NotificationDaysBefore: 3,
		CreatedAt:              time.Now().AddDate(0, -1, 0),
		Services:               map[string]config.ServiceConfig{"aws/rds": {Provider: "aws", Service: "rds"}},
	}, nil).Maybe()
	store.On("UpdatePurchasePlan", mock.Anything, mock.Anything).Return(nil).Maybe()

	return patchCSRFFixture{
		h:       &Handler{config: store, auth: m},
		session: session,
		mutated: func() bool { return mockMethodCalled(store, "UpdatePurchasePlan") },
	}
}

func ladderAmendCSRFFixture(t *testing.T) patchCSRFFixture {
	t.Helper()
	h, original := ladderScopedHandler(t)
	expectCSRF(h.auth.(*MockAuthService), scopedToken)
	store := &amendmentTestStore{MockConfigStore: original, accountID: scopedInAccount}
	h.config = store
	return patchCSRFFixture{h: h, session: scopedToken, mutated: func() bool { return store.called }}
}

// mockMethodCalled reports whether any recorded call on m used method.
func mockMethodCalled(m *MockConfigStore, method string) bool {
	for _, c := range m.Calls {
		if c.Method == method {
			return true
		}
	}
	return false
}

func patchCSRFRequest(row patchCSRFRow, session, csrf string) *events.LambdaFunctionURLRequest {
	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{
			"authorization": "Bearer " + session,
			"content-type":  "application/json",
		},
		Body: row.body,
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "PATCH", Path: row.path},
		},
	}
	if csrf != "" {
		req.Headers["x-csrf-token"] = csrf
	}
	return req
}

// TestHandleRequest_SessionPATCHRequiresCSRF drives both PATCH routes through
// the full HandleRequest path with a valid session. Without a CSRF token, or
// with a wrong one, the request is refused with 403 before the handler runs.
// With the issued token the edit still goes through, so a fix that refuses
// every PATCH also fails.
func TestHandleRequest_SessionPATCHRequiresCSRF(t *testing.T) {
	for _, row := range patchCSRFRows {
		for _, tc := range []struct{ name, csrf string }{{"no CSRF header", ""}, {"wrong CSRF header", patchCSRFOther}} {
			t.Run(row.name+"/"+tc.name, func(t *testing.T) {
				f := row.build(t)
				resp, err := f.h.HandleRequest(context.Background(), patchCSRFRequest(row, f.session, tc.csrf))
				require.NoError(t, err)
				assert.False(t, f.mutated(), "PATCH %s changed state for a session without a valid CSRF token", row.path)
				assert.Equal(t, 403, resp.StatusCode, resp.Body)
				assert.Contains(t, resp.Body, "CSRF validation failed")
			})
		}
		t.Run(row.name+"/valid CSRF header", func(t *testing.T) {
			f := row.build(t)
			resp, err := f.h.HandleRequest(context.Background(), patchCSRFRequest(row, f.session, patchCSRFGood))
			require.NoError(t, err)
			assert.Equal(t, 200, resp.StatusCode, resp.Body)
			assert.True(t, f.mutated(), "PATCH %s with a valid CSRF token must still apply the edit", row.path)
		})
	}
}

// TestRequiresCSRFValidation_SafeMethodsOnly pins the predicate: only GET,
// HEAD and OPTIONS skip CSRF. Unknown verbs are checked (fail closed).
func TestRequiresCSRFValidation_SafeMethodsOnly(t *testing.T) {
	h := &Handler{}
	for method, want := range map[string]bool{
		"GET": false, "HEAD": false, "OPTIONS": false,
		"POST": true, "PUT": true, "DELETE": true, "PATCH": true,
		"PROPFIND": true, "patch": true, "": true,
	} {
		assert.Equal(t, want, h.requiresCSRFValidation(method, "/api/plans/"+patchCSRFPlan), "method %q", method)
	}
	// The pre-session exemptions still hold for every unsafe method.
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		assert.False(t, h.requiresCSRFValidation(method, "/api/auth/login"), "%s /api/auth/login", method)
	}
}

// csrfUnsafeMethods are the verbs a route with no Method (it matches any verb)
// is probed with.
var csrfUnsafeMethods = []string{"POST", "PUT", "PATCH", "DELETE"}

func isCSRFSafeMethod(method string) bool {
	return method == "GET" || method == "HEAD" || method == "OPTIONS"
}

// csrfSamplePath turns a route pattern into a concrete path it matches.
func csrfSamplePath(r Route) string {
	if r.ExactPath != "" {
		return r.ExactPath
	}
	return r.PathPrefix + "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" + r.PathSuffix
}

// TestRouter_EveryStateChangingRouteRequiresCSRF reads the real router table.
// Every non-public route reachable with a non-safe method must be CSRF-checked
// by the middleware. AuthPublic routes are skipped: the middleware returns
// before the CSRF check for them, and authpublic_csrf_guard_test.go covers
// them per route.
func TestRouter_EveryStateChangingRouteRequiresCSRF(t *testing.T) {
	h := &Handler{}
	r := NewRouter(h)
	checked := map[string]bool{}
	for _, route := range r.routes {
		if route.Auth == AuthPublic {
			continue
		}
		methods := []string{route.Method}
		if route.Method == "" {
			methods = csrfUnsafeMethods
		}
		path := csrfSamplePath(route)
		require.True(t, r.matches(route, methods[0], path), "sample path %s does not match %s", path, csrfRouteKey(route))
		require.False(t, h.isPublicEndpoint(path), "%s is not AuthPublic but its path is treated as public", csrfRouteKey(route))
		for _, method := range methods {
			if isCSRFSafeMethod(method) {
				continue
			}
			checked[method] = true
			assert.True(t, h.requiresCSRFValidation(method, path),
				"%s changes state with a session but the middleware skips CSRF for it", csrfRouteKey(route))
		}
	}
	for _, method := range csrfUnsafeMethods {
		assert.True(t, checked[method], "no non-public %s route found; the guard is reading the wrong table", method)
	}
}
