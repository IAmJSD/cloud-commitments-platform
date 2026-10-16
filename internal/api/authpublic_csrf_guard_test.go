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

// Class-level CSRF guard for AuthPublic routes (issue #193).
//
// isPublicEndpoint makes validateSecurityContext return before
// requiresCSRFValidation is consulted, so the middleware never checks CSRF on
// an AuthPublic route. Any AuthPublic route that reads a session and then
// changes state has to call h.validateCSRF itself. Nothing enforced that for
// the class: approveRIExchangeViaSession shipped without the call (cli#1757)
// while a predicate-level test of the dead middleware table kept passing.
//
// The two tests below close that:
//
//   - TestAuthPublicRoutes_EveryRouteIsCSRFGuardedOrExempt reads the real
//     router table. Every AuthPublic route must have a row in
//     authPublicCSRFGuardRows or an entry, with a reason, in
//     authPublicCSRFExempt. A new AuthPublic route fails it until someone
//     makes that choice.
//   - TestAuthPublicMoneyRoutes_SessionWithoutCSRFIsRefused drives each row
//     through Router.Route, not the handler, with a valid session. Without a
//     CSRF header the money-moving call must not run. With a valid header it
//     must run, so a fix that breaks every session approve also fails.
//
// Each fixture registers its full happy path with .Maybe(). If a route's
// CSRF check is removed, the request runs on to the money-moving call and the
// AssertNotCalled fails. It does not panic on an earlier unmocked call.
//
// What the rows pin, per check:
//
//   - The shared pre-lookup check in requireSessionBeforeLookup
//     (handler_purchases.go) runs validateCSRF for every purchase row, so
//     removing it fails the purchase rows. The RI-exchange session check
//     (handler_ri_exchange.go, approveRIExchangeViaSession) fails the RI rows.
//   - The per-handler validateCSRF calls in approvePurchaseViaSession,
//     cancelPurchaseViaSession and tryRevokeViaSession sit behind that shared
//     check, so removing only one of them leaves every row green. They are
//     defense in depth; the guard deliberately pins the outer gate, not them.
//   - authPublicCSRFExempt is a per-route claim (for example, the GET revoke
//     and unsubscribe handlers don't read a session). If an exempt handler
//     later starts reading the session, this guard can't tell; the class
//     check only forces the choice for routes that are new to the table.

const (
	csrfGuardSession  = "csrf-guard-sess"
	csrfGuardHeader   = "csrf-guard-ok"
	csrfGuardExecID   = "6f1c2d3e-4a5b-4c6d-8e7f-901234567893"
	csrfGuardRIID     = "6f1c2d3e-4a5b-4c6d-8e7f-901234567894"
	csrfGuardRawToken = "csrf-guard-email-token"
	csrfGuardContact  = "contact@example.com"
)

// authPublicCSRFExempt lists the AuthPublic routes that need no CSRF guard
// row, keyed by csrfRouteKey. Each entry says why. Add a route here only if it
// never acts on the caller's session credential.
var authPublicCSRFExempt = map[string]string{
	"GET /api/purchases/revoke/*":         "renders the confirmation form only; revokeViaEmailToken returns before reading the session",
	"POST /api/ri-exchange/reject/*":      "token-only; rejectRIExchange takes no request, so it cannot read a session",
	"POST /api/auth/login":                "pre-session: issues the session, so there is no session to protect yet",
	"GET /api/auth/check-admin":           "read-only",
	"POST /api/auth/setup-admin":          "pre-session: first-admin bootstrap",
	"POST /api/auth/forgot-password":      "pre-session: sends a reset email",
	"POST /api/auth/reset-password":       "pre-session: authorized by the reset token",
	"GET /api/auth/reset-password/status": "read-only",
	"GET /api/notifications/unsubscribe":  "HMAC-signed token only; never reads the session and moves no money",
	"POST /api/notifications/unsubscribe": "HMAC-signed token only (RFC 8058); never reads the session and moves no money",
	"POST /api/register":                  "public registration; creates a pending request an admin must approve",
	"GET /api/register/*":                 "read-only",
	"ANY /health":                         "read-only health check",
	"ANY /api/health":                     "read-only health check",
	"GET /api/info":                       "read-only",
	"GET /version":                        "read-only",
	"GET /api/docs*":                      "read-only",
	"HEAD /api/docs*":                     "read-only",
	"GET /docs*":                          "read-only",
	"HEAD /docs*":                         "read-only",
}

// csrfGuardFixture is one route's handler plus the checks for both
// directions: refused (no CSRF header) and allowed (valid header).
type csrfGuardFixture struct {
	h       *Handler
	refused func(t *testing.T)
	allowed func(t *testing.T)
}

type csrfGuardRow struct {
	name   string
	route  string // csrfRouteKey of the AuthPublic route this row must dispatch to
	method string
	path   string
	token  string
	build  func(t *testing.T) csrfGuardFixture
}

// authPublicCSRFGuardRows are the AuthPublic routes with a session-authed
// branch that reaches a state-changing or money-moving call. The token rows
// use a signed-in contact-email recipient with no approve/cancel RBAC, so the
// dispatch falls through to the email-link branch, which also needs CSRF when
// a session is present (issue #597).
var authPublicCSRFGuardRows = []csrfGuardRow{
	{"approve/session", "POST /api/purchases/approve/*", "POST", "/api/purchases/approve/" + csrfGuardExecID, "", purchaseCSRFGuardFixture("approve", false)},
	{"approve/token", "POST /api/purchases/approve/*", "POST", "/api/purchases/approve/" + csrfGuardExecID, csrfGuardRawToken, purchaseCSRFGuardFixture("approve", true)},
	{"cancel/session", "POST /api/purchases/cancel/*", "POST", "/api/purchases/cancel/" + csrfGuardExecID, "", purchaseCSRFGuardFixture("cancel", false)},
	{"cancel/token", "POST /api/purchases/cancel/*", "POST", "/api/purchases/cancel/" + csrfGuardExecID, csrfGuardRawToken, purchaseCSRFGuardFixture("cancel", true)},
	{"revoke/session", "POST /api/purchases/revoke/*", "POST", "/api/purchases/revoke/" + csrfGuardExecID, "", purchaseCSRFGuardFixture("revoke", false)},
	{"revoke/token", "POST /api/purchases/revoke/*", "POST", "/api/purchases/revoke/" + csrfGuardExecID, csrfGuardRawToken, purchaseCSRFGuardFixture("revoke", true)},
	{"ri-approve/session", "POST /api/ri-exchange/approve/*", "POST", "/api/ri-exchange/approve/" + csrfGuardRIID, "", riApproveCSRFGuardFixture},
	{"ri-approve/session+token", "POST /api/ri-exchange/approve/*", "POST", "/api/ri-exchange/approve/" + csrfGuardRIID, "tok", riApproveCSRFGuardFixture},
}

// csrfRouteKey names a route by method and path pattern ("*" marks the
// prefix's variable part), e.g. "POST /api/purchases/approve/*".
func csrfRouteKey(r Route) string {
	method := r.Method
	if method == "" {
		method = "ANY"
	}
	if r.ExactPath != "" {
		return method + " " + r.ExactPath
	}
	return method + " " + r.PathPrefix + "*" + r.PathSuffix
}

func TestAuthPublicRoutes_EveryRouteIsCSRFGuardedOrExempt(t *testing.T) {
	guarded := map[string]bool{}
	for _, row := range authPublicCSRFGuardRows {
		guarded[row.route] = true
	}

	r := NewRouter(&Handler{})
	public := map[string]bool{}
	for _, route := range r.routes {
		if route.Auth != AuthPublic {
			continue
		}
		key := csrfRouteKey(route)
		public[key] = true
		_, exempt := authPublicCSRFExempt[key]
		switch {
		case guarded[key] && exempt:
			t.Errorf("AuthPublic route %s is both guarded and exempt; pick one", key)
		case !guarded[key] && !exempt:
			t.Errorf("AuthPublic route %s is not classified. The middleware skips CSRF for AuthPublic routes, "+
				"so if it reads a session and changes state its handler must call h.validateCSRF and it needs a row "+
				"in authPublicCSRFGuardRows. Otherwise add it to authPublicCSRFExempt with the reason.", key)
		}
	}
	require.NotEmpty(t, public, "router table has no AuthPublic routes; the guard is reading the wrong table")

	for key := range guarded {
		assert.True(t, public[key], "guard row targets %s, which is not an AuthPublic route", key)
	}
	for key := range authPublicCSRFExempt {
		assert.True(t, public[key], "exemption %s names no AuthPublic route; remove it", key)
	}
}

func TestAuthPublicMoneyRoutes_SessionWithoutCSRFIsRefused(t *testing.T) {
	for _, row := range authPublicCSRFGuardRows {
		t.Run(row.name+"/no CSRF header", func(t *testing.T) {
			f := row.build(t)
			_, err := routeCSRFGuardRow(t, f.h, row, false)
			// The money-moving call is the primary check: an error alone does
			// not prove the state change was prevented.
			f.refused(t)
			if t.Failed() {
				t.Errorf("%s (%s): a money-moving call ran for a session without a CSRF header", row.route, row.name)
			}
			require.ErrorIs(t, err, errCSRFRejected, "%s: a session without a CSRF header must get 403 CSRF validation failed", row.route)
		})
		t.Run(row.name+"/valid CSRF header", func(t *testing.T) {
			f := row.build(t)
			_, err := routeCSRFGuardRow(t, f.h, row, true)
			require.NoError(t, err, "%s: a session with a valid CSRF header must still succeed", row.route)
			f.allowed(t)
		})
	}
}

// routeCSRFGuardRow checks that the row's path dispatches to the route the row
// names, then sends a session-bearing request through Router.Route.
func routeCSRFGuardRow(t *testing.T, h *Handler, row csrfGuardRow, withCSRF bool) (any, error) {
	t.Helper()
	r := NewRouter(h)
	var dispatched *Route
	for i := range r.routes {
		if r.matches(r.routes[i], row.method, row.path) {
			dispatched = &r.routes[i]
			break
		}
	}
	require.NotNil(t, dispatched, "%s %s matches no route", row.method, row.path)
	require.Equal(t, row.route, csrfRouteKey(*dispatched), "%s %s dispatches to another route", row.method, row.path)
	require.Equal(t, AuthPublic, dispatched.Auth, "%s is no longer AuthPublic", row.route)

	req := &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"authorization": "Bearer " + csrfGuardSession},
		QueryStringParameters: map[string]string{"token": row.token},
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: row.method, Path: row.path},
		},
	}
	if withCSRF {
		req.Headers["x-csrf-token"] = csrfGuardHeader
	}
	return r.Route(context.Background(), row.method, row.path, req)
}

// csrfGuardAuth answers the session and both CSRF cases. Permissions are
// granted by the caller.
func csrfGuardAuth(email string) *MockAuthService {
	m := new(MockAuthService)
	m.On("ValidateSession", mock.Anything, csrfGuardSession).Return(&Session{UserID: "caller-user-id", Email: email}, nil)
	m.On("ValidateCSRFToken", mock.Anything, csrfGuardSession, csrfGuardHeader).Return(nil).Maybe()
	m.On("ValidateCSRFToken", mock.Anything, csrfGuardSession, "").Return(errors.New("missing CSRF token")).Maybe()
	return m
}

// purchaseCSRFGuardFixture builds approve, cancel or revoke on an execution
// owned by acct-1 (contact csrfGuardContact). viaToken selects the email-link
// branch: the caller is the contact recipient with no RBAC. Otherwise the
// caller is an admin purchaser and takes the session branch.
func purchaseCSRFGuardFixture(action string, viaToken bool) func(t *testing.T) csrfGuardFixture {
	return func(t *testing.T) csrfGuardFixture {
		t.Helper()
		accountID := "acct-1"
		creator := "creator-user-id"
		contact := csrfGuardContact
		future := time.Now().Add(time.Hour)
		status := "pending"
		if action == "revoke" {
			status = "completed"
		}
		exec := &config.PurchaseExecution{
			ExecutionID:            csrfGuardExecID,
			Status:                 status,
			ApprovalToken:          config.HashApprovalToken(csrfGuardRawToken),
			ApprovalTokenExpiresAt: &future,
			CloudAccountID:         &accountID,
			CreatedByUserID:        &creator,
			Recommendations:        []config.RecommendationRecord{{ID: "r1", Provider: "aws", CloudAccountID: &accountID, UpfrontCost: 100}},
		}

		store := new(MockConfigStore)
		store.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
			return &config.CloudAccount{ID: id, Name: id, Provider: "aws", ContactEmail: contact}, nil
		}
		store.On("GetExecutionByID", mock.Anything, csrfGuardExecID).Return(exec, nil).Maybe()
		store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{NotificationEmail: &contact}, nil).Maybe()
		store.On("TransitionExecutionStatus", mock.Anything, csrfGuardExecID, mock.Anything, mock.Anything, mock.Anything).Return(exec, nil).Maybe()
		store.On("SetCancelledBy", mock.Anything, csrfGuardExecID, mock.Anything).Return(nil).Maybe()

		pm := new(MockPurchaseManager)
		pm.On("ApproveAndExecute", mock.Anything, csrfGuardExecID, mock.Anything, mock.Anything).Return(exec, "", nil).Maybe()
		pm.On("ApproveExecution", mock.Anything, csrfGuardExecID, csrfGuardRawToken, mock.Anything).Return("", nil).Maybe()
		pm.On("CancelExecution", mock.Anything, csrfGuardExecID, csrfGuardRawToken, mock.Anything).Return(nil).Maybe()

		var m *MockAuthService
		if viaToken {
			m = csrfGuardAuth(contact)
			m.grantPermissions(nil)
		} else {
			m = csrfGuardAuth("admin@example.com")
			m.grantAdminPurchaser()
		}

		refused := func(t *testing.T) {
			t.Helper()
			pm.AssertNotCalled(t, "ApproveAndExecute", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			pm.AssertNotCalled(t, "ApproveExecution", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			pm.AssertNotCalled(t, "CancelExecution", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			store.AssertNotCalled(t, "CancelExecutionAtomic")
			store.AssertNotCalled(t, "DeleteSuppressionsByExecutionTx")
			store.AssertNotCalled(t, "TransitionExecutionStatus")
			store.AssertNotCalled(t, "SetCancelledBy")
		}
		allowed := func(t *testing.T) {
			t.Helper()
			switch {
			case action == "approve" && !viaToken:
				pm.AssertCalled(t, "ApproveAndExecute", mock.Anything, csrfGuardExecID, mock.Anything, mock.Anything)
			case action == "approve":
				pm.AssertCalled(t, "ApproveExecution", mock.Anything, csrfGuardExecID, csrfGuardRawToken, contact)
			case action == "cancel" && !viaToken:
				store.AssertCalled(t, "CancelExecutionAtomic")
			case action == "cancel":
				pm.AssertCalled(t, "CancelExecution", mock.Anything, csrfGuardExecID, csrfGuardRawToken, contact)
			case action == "revoke":
				store.AssertCalled(t, "TransitionExecutionStatus")
			default:
				t.Fatalf("no money-moving call defined for action %q", action)
			}
		}
		return csrfGuardFixture{h: &Handler{config: store, auth: m, purchase: pm}, refused: refused, allowed: allowed}
	}
}

// riApproveCSRFGuardFixture builds an RI exchange approve by an admin
// purchaser, which takes the session branch with or without a token.
func riApproveCSRFGuardFixture(t *testing.T) csrfGuardFixture {
	t.Helper()
	creatorID := "creator-uuid"
	store := new(MockConfigStore)
	store.On("GetRIExchangeRecord", mock.Anything, csrfGuardRIID).Return(&config.RIExchangeRecord{
		ID:              csrfGuardRIID,
		Status:          "pending",
		ApprovalToken:   "tok",
		SourceRIIDs:     []string{"ri-123"},
		PaymentDue:      "100.00",
		CreatedByUserID: &creatorID,
	}, nil).Maybe()
	store.On("TransitionRIExchangeStatus", mock.Anything, csrfGuardRIID, "pending", "processing", mock.Anything).
		Return(&config.RIExchangeRecord{ID: csrfGuardRIID, Status: "processing", SourceRIIDs: []string{"ri-123"}, PaymentDue: "100.00"}, nil).Maybe()
	store.On("ReserveApprovedRIExchange", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return("500.000000", nil).Maybe()
	store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{
		RIExchangeMaxDailyUSD:       1000,
		RIExchangeMaxPerExchangeUSD: 500,
	}, nil).Maybe()
	store.On("FailRIExchange", mock.Anything, csrfGuardRIID, mock.AnythingOfType("string")).Return(nil).Maybe()
	store.On("StampRIExchangeApprovedBy", mock.Anything, csrfGuardRIID, mock.Anything).Return(nil).Maybe()

	m := csrfGuardAuth("admin@example.com")
	m.grantAdminPurchaser()

	refused := func(t *testing.T) {
		t.Helper()
		store.AssertNotCalled(t, "TransitionRIExchangeStatus")
		store.AssertNotCalled(t, "ReserveApprovedRIExchange")
		store.AssertNotCalled(t, "StampRIExchangeApprovedBy")
	}
	allowed := func(t *testing.T) {
		t.Helper()
		store.AssertCalled(t, "TransitionRIExchangeStatus", mock.Anything, csrfGuardRIID, "pending", "processing", mock.Anything)
	}
	return csrfGuardFixture{h: &Handler{config: store, auth: m}, refused: refused, allowed: allowed}
}
