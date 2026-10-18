package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// The approval deep link's confirm modal loads the purchase details before it
// offers Approve. These tests drive POST /api/purchases/approve/{id}/details
// through HandleRequest with the oracle fixture: execution in acct-1, contact
// approver contact@example.com, created by someone else (issue #529).

func approvalDetailsRequest(method, token, csrf string) *events.LambdaFunctionURLRequest {
	headers := map[string]string{"authorization": "Bearer sess-tok"}
	if csrf != "" {
		headers["x-csrf-token"] = csrf
	}
	body := ""
	if token != "" {
		b, _ := json.Marshal(map[string]string{"token": token})
		body = string(b)
		headers["content-type"] = "application/json"
	}
	return &events.LambdaFunctionURLRequest{
		Headers: headers,
		Body:    body,
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{
				Method: method,
				Path:   "/api/purchases/approve/" + tokenErrExecID + "/details",
			},
		},
	}
}

func viewOnlyCaller(email string) oracleCaller {
	return oracleCaller{
		grant: func(m *MockAuthService) {
			m.grantPermissions([]auth.Permission{{Action: auth.ActionView, Resource: auth.ResourcePurchases}})
		},
		email:    email,
		sendCSRF: true,
	}
}

func contactCaller() oracleCaller {
	return oracleCaller{grant: func(m *MockAuthService) { m.grantPermissions(nil) }, email: "contact@example.com", sendCSRF: true}
}

func pendingApprovalExec(expiresAt time.Time) *config.PurchaseExecution {
	return &config.PurchaseExecution{
		Status:                 "pending",
		ApprovalToken:          config.HashApprovalToken(tokenErrRawToken),
		ApprovalTokenExpiresAt: &expiresAt,
		TotalUpfrontCost:       100,
	}
}

func callApprovalDetails(t *testing.T, h *Handler, caller oracleCaller, token string) *events.LambdaFunctionURLResponse {
	t.Helper()
	csrf := ""
	if caller.sendCSRF {
		csrf = oracleCSRF
	}
	resp, err := h.HandleRequest(context.Background(), approvalDetailsRequest("POST", token, csrf))
	require.NoError(t, err)
	return resp
}

// assertReadOnly fails if the details read changed execution state.
func assertReadOnly(t *testing.T, h *Handler) {
	t.Helper()
	store := h.config.(*MockConfigStore)
	store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	store.AssertNotCalled(t, "CancelExecutionAtomic", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// A contact approver without view:purchases is denied by GET
// /api/purchases/{id}, but can read the details on the approval link with
// the email token (issue #529).
func TestHandleRequest_ApprovalDetails_ContactApproverWithoutViewPurchases(t *testing.T) {
	future := time.Now().Add(time.Hour)
	caller := contactCaller()

	// The generic details route still refuses this caller: nothing changed there.
	h := oracleHandler(t, caller, pendingApprovalExec(future), false)
	getReq := approvalDetailsRequest("GET", "", "")
	getReq.RequestContext.HTTP.Path = "/api/purchases/" + tokenErrExecID
	getResp, err := h.HandleRequest(context.Background(), getReq)
	require.NoError(t, err)
	require.Equal(t, 403, getResp.StatusCode, getResp.Body)

	h = oracleHandler(t, caller, pendingApprovalExec(future), false)
	resp := callApprovalDetails(t, h, caller, tokenErrRawToken)
	require.Equal(t, 200, resp.StatusCode, resp.Body)
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &body))
	assert.Equal(t, tokenErrExecID, body["execution_id"])
	assert.InDelta(t, 100.0, body["total_upfront_cost"], 0.001)
	recs, ok := body["recommendations"].([]any)
	require.True(t, ok, "recommendations missing: %s", resp.Body)
	assert.Len(t, recs, 1)
	assertReadOnly(t, h)
}

// The token alone, or the contact email alone, never authorizes the read.
func TestHandleRequest_ApprovalDetails_TokenAndContactBothRequired(t *testing.T) {
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)
	cases := []struct {
		name     string
		caller   oracleCaller
		exec     *config.PurchaseExecution
		token    string
		wantCode int
	}{
		{"contact, wrong token", contactCaller(), pendingApprovalExec(future), "wrong", 404},
		{"contact, no token", contactCaller(), pendingApprovalExec(future), "", 404},
		{"contact, execution has no token", contactCaller(), &config.PurchaseExecution{Status: "pending"}, tokenErrRawToken, 404},
		{"contact, expired token", contactCaller(), pendingApprovalExec(past), tokenErrRawToken, 410},
		{"not the contact, valid token", oracleCaller{grant: func(m *MockAuthService) { m.grantPermissions(nil) }, email: "outsider@example.com", sendCSRF: true}, pendingApprovalExec(future), tokenErrRawToken, 404},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := oracleHandler(t, tc.caller, tc.exec, false)
			resp := callApprovalDetails(t, h, tc.caller, tc.token)
			assert.Equal(t, tc.wantCode, resp.StatusCode, resp.Body)
			assert.NotContains(t, resp.Body, "recommendations")
			if tc.wantCode == 404 {
				assert.Contains(t, resp.Body, "execution not found")
			}
		})
	}
}

// A session with view:purchases in scope reads the details without a token,
// exactly as GET /api/purchases/{id} allows; out of scope it needs the
// contact gate like everyone else.
func TestHandleRequest_ApprovalDetails_ViewPurchasesSession(t *testing.T) {
	future := time.Now().Add(time.Hour)

	t.Run("in scope, no token", func(t *testing.T) {
		caller := viewOnlyCaller("viewer@example.com")
		h := oracleHandler(t, caller, pendingApprovalExec(future), false)
		resp := callApprovalDetails(t, h, caller, "")
		assert.Equal(t, 200, resp.StatusCode, resp.Body)
	})
	t.Run("scoped out, not the contact, valid token", func(t *testing.T) {
		caller := oracleCaller{grant: func(m *MockAuthService) { m.grantScoped("other-acct") }, email: "outsider@example.com", sendCSRF: true}
		h := oracleHandler(t, caller, pendingApprovalExec(future), false)
		resp := callApprovalDetails(t, h, caller, tokenErrRawToken)
		assert.Equal(t, 404, resp.StatusCode, resp.Body)
	})
	t.Run("scoped out, contact, valid token", func(t *testing.T) {
		caller := oracleCaller{grant: func(m *MockAuthService) { m.grantScoped("other-acct") }, email: "contact@example.com", sendCSRF: true}
		h := oracleHandler(t, caller, pendingApprovalExec(future), false)
		resp := callApprovalDetails(t, h, caller, tokenErrRawToken)
		assert.Equal(t, 200, resp.StatusCode, resp.Body)
	})
}

// No session is 401 and a session without CSRF is 403, both before the
// execution is read (issues #435, #597).
func TestHandleRequest_ApprovalDetails_SessionAndCSRFBeforeLookup(t *testing.T) {
	future := time.Now().Add(time.Hour)

	t.Run("no session", func(t *testing.T) {
		h := oracleHandler(t, contactCaller(), pendingApprovalExec(future), false)
		req := approvalDetailsRequest("POST", tokenErrRawToken, oracleCSRF)
		delete(req.Headers, "authorization")
		resp, err := h.HandleRequest(context.Background(), req)
		require.NoError(t, err)
		assert.Equal(t, 401, resp.StatusCode, resp.Body)
		store := h.config.(*MockConfigStore)
		store.AssertNotCalled(t, "GetExecutionByID", mock.Anything, mock.Anything)
	})
	t.Run("contact without CSRF", func(t *testing.T) {
		caller := contactCaller()
		caller.sendCSRF = false
		h := oracleHandler(t, caller, pendingApprovalExec(future), false)
		resp := callApprovalDetails(t, h, caller, tokenErrRawToken)
		assert.Equal(t, 403, resp.StatusCode, resp.Body)
		assert.Contains(t, resp.Body, "CSRF")
		store := h.config.(*MockConfigStore)
		store.AssertNotCalled(t, "GetExecutionByID", mock.Anything, mock.Anything)
	})
}

// A signed-in caller who may not read the details gets the same status,
// headers and body for an existing and a missing execution (issue #597).
func TestHandleRequest_ApprovalDetails_SignedInCannotTellMissingFromExisting(t *testing.T) {
	future := time.Now().Add(time.Hour)
	for _, status := range []string{"pending", "completed", "orphan"} {
		for _, token := range []string{"", tokenErrRawToken, "wrong"} {
			for name, caller := range oracleCallers() {
				if name == "contact/no-csrf" && token == "" {
					continue
				}
				t.Run(status+"/token="+token+"/"+name, func(t *testing.T) {
					orphan := status == "orphan"
					exec := pendingApprovalExec(future)
					if !orphan {
						exec.Status = status
					}
					existingH := oracleHandler(t, caller, exec, orphan)
					existing := callApprovalDetails(t, existingH, caller, token)
					missing := callApprovalDetails(t, oracleHandler(t, caller, nil, false), caller, token)

					assert.Contains(t, []int{403, 404}, existing.StatusCode, existing.Body)
					assert.Equal(t, missing.StatusCode, existing.StatusCode, "existing: %s / missing: %s", existing.Body, missing.Body)
					assert.Equal(t, missing.Body, existing.Body)
					assert.Equal(t, missing.Headers, existing.Headers)
					assertReadOnly(t, existingH)
				})
			}
		}
	}
}

// The details route must not shadow the approve route it shares a prefix
// with: POST /api/purchases/approve/{id} still reaches approvePurchase.
func TestRouter_ApprovalDetailsDoesNotShadowApprove(t *testing.T) {
	r := &Router{}
	r.registerRoutes()
	match := func(path string) Route {
		for _, route := range r.routes {
			if r.matches(route, "POST", path) {
				return route
			}
		}
		t.Fatalf("no route for %s", path)
		return Route{}
	}
	details := match("/api/purchases/approve/" + tokenErrExecID + "/details")
	assert.Equal(t, "/details", details.PathSuffix)
	assert.Equal(t, tokenErrExecID, r.extractParams(details, "/api/purchases/approve/"+tokenErrExecID+"/details")["id"])
	approve := match("/api/purchases/approve/" + tokenErrExecID)
	assert.Empty(t, approve.PathSuffix)
}
