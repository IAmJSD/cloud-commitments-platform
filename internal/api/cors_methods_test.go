package api

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CORS method list vs the router (issue #310, finding A02-015).
//
// buildResponseHeaders advertised GET, POST, PUT, DELETE and OPTIONS, but the
// router also serves PATCH (PATCH /api/plans/{id}, PATCH
// /api/ladder/tranches/{id}). A cross-origin deployment's preflight for those
// routes was answered without PATCH, so the browser refused the real request.

// preflightAllowedMethods sends an OPTIONS preflight for path to a handler
// with a configured CORS origin and returns the advertised methods.
func preflightAllowedMethods(t *testing.T, h *Handler, path string) map[string]bool {
	t.Helper()
	resp, err := h.HandleRequest(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: map[string]string{
			"origin":                        h.corsAllowedOrigin,
			"access-control-request-method": "PATCH",
		},
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "OPTIONS", Path: path},
		},
	})
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode, resp.Body)
	header := resp.Headers["Access-Control-Allow-Methods"]
	require.NotEmpty(t, header, "a handler with a CORS origin must advertise its methods")
	allowed := map[string]bool{}
	for _, m := range strings.Split(header, ",") {
		allowed[strings.TrimSpace(m)] = true
	}
	return allowed
}

// TestHandleRequest_PreflightAllowsPATCH is the issue's scenario: the
// preflight for PATCH /api/plans/{id} must list PATCH.
func TestHandleRequest_PreflightAllowsPATCH(t *testing.T) {
	h := &Handler{corsAllowedOrigin: "https://dashboard.example.com"}
	allowed := preflightAllowedMethods(t, h, "/api/plans/"+patchCSRFPlan)
	assert.True(t, allowed["PATCH"], "preflight for PATCH /api/plans/{id} does not allow PATCH: %v", allowed)
}

// TestHandleRequest_PreflightAllowsEveryRoutedMethod reads the real router
// table: every verb a route is registered with must be in the advertised
// list, so a verb a route adds later cannot silently fail cross-origin.
func TestHandleRequest_PreflightAllowsEveryRoutedMethod(t *testing.T) {
	h := &Handler{corsAllowedOrigin: "https://dashboard.example.com"}
	allowed := preflightAllowedMethods(t, h, "/api/plans/"+patchCSRFPlan)

	routed := map[string]bool{}
	for _, route := range NewRouter(h).routes {
		// A route with no Method matches any verb and is covered by the
		// concrete verbs below. HEAD is a CORS-safelisted method: browsers
		// never preflight it, so it need not be advertised.
		if route.Method == "" || route.Method == "HEAD" {
			continue
		}
		routed[route.Method] = true
		assert.True(t, allowed[route.Method],
			"%s is routed with %s but the CORS preflight does not allow it", csrfRouteKey(route), route.Method)
	}
	// Floor: if the table read finds no PATCH route, the guard is reading the
	// wrong table and would pass without checking anything.
	for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
		assert.True(t, routed[m], "no %s route found in the router table", m)
	}
	assert.True(t, allowed["OPTIONS"], "preflight must keep allowing OPTIONS")
}
