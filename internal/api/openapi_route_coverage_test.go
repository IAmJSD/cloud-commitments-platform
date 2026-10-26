package api

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// Issue #262: openapi.yaml had drifted from router.go, leaving about a third
// of the routed paths undocumented. That hides them from API consumers and
// also exempts them from TestOpenAPI403OnPermissionGatedRoutes, which only
// knows the routes the spec lists. This guard keeps the two in lockstep in
// both directions:
//
//   - every registered route must be reached by at least one documented
//     operation, and
//   - every documented operation must dispatch to a registered route.
//
// "Reached" is decided by the live router itself: each spec path is
// instantiated with a sample value per path parameter and handed to the same
// first-match logic Router.Route uses, so prefix/suffix ordering and
// multi-segment parameters behave exactly as they do in production.
//
// Scope and limits:
//
//   - Only routes from Router.registerRoutes are walked. Paths the server mux
//     handles before the API router (internal/server/http.go, such as /oidc/*
//     and /api/scheduled/*) are outside this guard.
//   - Parameter names and segment counts under a prefix route are not
//     checked. Every {param} becomes the same sample value and a prefix route
//     accepts any remainder, so renaming {provider}/{service} to {service}
//     still reaches the same route. The guard catches missing, extra and
//     wrong-method operations, not misnamed parameters.

// routeCoverageSample stands in for every {param} segment of a spec path.
const routeCoverageSample = "x"

// specOnlyOperations are documented operations deliberately served outside
// the API router. Each entry needs a reason.
var specOnlyOperations = map[string]string{
	"GET /ready": "served by internal/server/http.go before the API router (the Lambda transport has no readiness probe)",
}

// undocumentedRoutes are registered routes whose documentation lands
// elsewhere. Keys use routeCoveragePattern. Each entry needs a reason, and an
// entry fails the test once the route is documented, so it cannot go stale.
// It is empty: every registered route is documented.
var undocumentedRoutes = map[string]string{}

var specPathParam = regexp.MustCompile(`\{[^/{}]+\}`)

// routeCoveragePattern renders a route as "METHOD path", with "*" for the
// variable part of a prefix route and "ANY" for a method-less route.
func routeCoveragePattern(route Route) string {
	method := route.Method
	if method == "" {
		method = "ANY"
	}
	if route.ExactPath != "" {
		return method + " " + route.ExactPath
	}
	return method + " " + route.PathPrefix + "*" + route.PathSuffix
}

// specOperationKeys returns "METHOD /path" for every operation in the spec.
func specOperationKeys(t *testing.T, spec []byte) []string {
	t.Helper()
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	require.NoError(t, yaml.Unmarshal(spec, &doc))
	require.NotEmpty(t, doc.Paths, "spec has no paths")

	methods := map[string]bool{
		"get": true, "put": true, "post": true, "delete": true,
		"options": true, "head": true, "patch": true, "trace": true,
	}
	var keys []string
	for path, item := range doc.Paths {
		for method := range item {
			if methods[method] {
				keys = append(keys, strings.ToUpper(method)+" "+path)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

// dispatchIndex returns the index of the route Router.Route would pick for
// method and path, or -1 when none matches.
func dispatchIndex(r *Router, method, path string) int {
	for i, route := range r.routes {
		if r.matches(route, method, path) {
			return i
		}
	}
	return -1
}

// routeCoverageGaps compares a router's routes with a spec's operations and
// returns the routes no operation reaches and the operations no route serves.
func routeCoverageGaps(t *testing.T, r *Router, spec []byte) (undocumented, unrouted []string) {
	t.Helper()
	covered := make([]bool, len(r.routes))
	for _, key := range specOperationKeys(t, spec) {
		method, path, _ := strings.Cut(key, " ")
		concrete := specPathParam.ReplaceAllString(path, routeCoverageSample)
		idx := dispatchIndex(r, method, concrete)
		if idx < 0 {
			unrouted = append(unrouted, key)
			continue
		}
		covered[idx] = true
	}
	for i, route := range r.routes {
		if !covered[i] {
			undocumented = append(undocumented, routeCoveragePattern(route))
		}
	}
	return undocumented, unrouted
}

// partitionExempt splits found into entries missing from exempt (real gaps)
// and exempt entries that are absent from found (stale exemptions).
func partitionExempt(found []string, exempt map[string]string) (gaps, stale []string) {
	seen := make(map[string]bool, len(found))
	for _, f := range found {
		seen[f] = true
		if _, ok := exempt[f]; !ok {
			gaps = append(gaps, f)
		}
	}
	for e := range exempt {
		if !seen[e] {
			stale = append(stale, e)
		}
	}
	sort.Strings(stale)
	return gaps, stale
}

func newRouteCoverageRouter() *Router {
	r := &Router{h: &Handler{}}
	r.registerRoutes()
	return r
}

func TestOpenAPIDocumentsEveryRoute(t *testing.T) {
	r := newRouteCoverageRouter()
	require.NotEmpty(t, r.routes)

	undocumented, unrouted := routeCoverageGaps(t, r, openapiSpec)

	gaps, stale := partitionExempt(undocumented, undocumentedRoutes)
	assert.Empty(t, gaps,
		"routes registered in router.go with no operation in openapi.yaml; document them:\n%s",
		strings.Join(gaps, "\n"))
	assert.Empty(t, stale,
		"undocumentedRoutes entries that are now documented (or no longer routed); delete them:\n%s",
		strings.Join(stale, "\n"))

	gaps, stale = partitionExempt(unrouted, specOnlyOperations)
	assert.Empty(t, gaps,
		"operations in openapi.yaml that no route in router.go serves; fix the path or method, or remove them:\n%s",
		strings.Join(gaps, "\n"))
	assert.Empty(t, stale,
		"specOnlyOperations entries that the router now serves (or the spec no longer lists); delete them:\n%s",
		strings.Join(stale, "\n"))
}

// The checker itself must notice drift in each direction; otherwise a bug in
// it would let TestOpenAPIDocumentsEveryRoute pass vacuously.
func TestRouteCoverageGapsDetectsDrift(t *testing.T) {
	r := &Router{routes: []Route{
		{ExactPath: "/api/a", Method: "GET"},
		{PathPrefix: "/api/b/", PathSuffix: "/sub", Method: "POST"},
		{PathPrefix: "/api/b/", Method: "GET"},
		{ExactPath: "/health"},
	}}
	spec := []byte(`
paths:
  /api/a:
    get: {}
    post: {}
  /api/b/{id}/sub:
    post: {}
  /health:
    get: {}
`)
	undocumented, unrouted := routeCoverageGaps(t, r, spec)
	assert.Equal(t, []string{"GET /api/b/*"}, undocumented)
	assert.Equal(t, []string{"POST /api/a"}, unrouted)

	// A multi-segment parameter reaches the prefix route, as it does live.
	spec = []byte(`
paths:
  /api/a:
    get: {}
  /api/b/{id}/sub:
    post: {}
  /api/b/{provider}/{service}:
    get: {}
  /health:
    get: {}
`)
	undocumented, unrouted = routeCoverageGaps(t, r, spec)
	assert.Empty(t, undocumented)
	assert.Empty(t, unrouted)
}

func TestPartitionExemptReportsStaleEntries(t *testing.T) {
	gaps, stale := partitionExempt(
		[]string{"GET /a", "GET /b"},
		map[string]string{"GET /b": "reason", "GET /gone": "reason"},
	)
	assert.Equal(t, []string{"GET /a"}, gaps)
	assert.Equal(t, []string{"GET /gone"}, stale)
}
