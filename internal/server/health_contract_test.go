package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// topLevelHealthStatus decodes only the top-level "status" field, the way the
// post-deploy check (jsondecode(body).status) and the GCP uptime check
// (JSON path $.status) read it. It deliberately does not decode into
// HealthStatus, so renaming the JSON tag breaks this test.
func topLevelHealthStatus(t *testing.T, body []byte) string {
	t.Helper()
	var doc map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &doc), "/health body must be a JSON object: %s", body)
	raw, ok := doc["status"]
	require.True(t, ok, "/health body must carry a top-level \"status\" field: %s", body)
	var status string
	require.NoError(t, json.Unmarshal(raw, &status), "top-level \"status\" must be a string: %s", body)
	return status
}

// TestHealthBodyStatusIsTheVerdict pins the /health contract that
// terraform/modules/deployment-checks and the GCP uptime check in
// terraform/modules/monitoring/gcp gate on (#649): /health answers 200 even
// when degraded (liveness, #488), so the only verdict a caller can rely on is
// the top-level "status" field, which must read "healthy" only when the
// replica is fully healthy, on both the server mux and the Lambda router.
func TestHealthBodyStatusIsTheVerdict(t *testing.T) {
	serveMux := func(t *testing.T, app *Application) (int, []byte) {
		t.Helper()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
		w := httptest.NewRecorder()
		CreateHTTPServer(app, 0).Handler.ServeHTTP(w, req)
		return w.Code, w.Body.Bytes()
	}

	t.Run("server mux healthy", func(t *testing.T) {
		code, body := serveMux(t, &Application{
			Version: "test",
			Config:  &mockConfigStoreForHealth{},
			Auth:    createHealthyAuthService(),
		})
		assert.Equal(t, http.StatusOK, code)
		assert.Equal(t, "healthy", topLevelHealthStatus(t, body))
	})

	t.Run("server mux degraded still answers 200", func(t *testing.T) {
		// Auth store reachable, config store missing: a nested check still
		// reads "healthy", which is why a substring match on the body is not
		// a verdict.
		code, body := serveMux(t, &Application{
			Version: "test",
			Auth:    createHealthyAuthService(),
		})
		assert.Equal(t, http.StatusOK, code, "/health is the liveness contract and must stay 200")
		assert.Equal(t, "degraded", topLevelHealthStatus(t, body))
		assert.Contains(t, string(body), `"healthy"`,
			"a degraded body contains the string \"healthy\" in its nested checks; gates must read the top-level field")
	})

	t.Run("lambda router degraded still answers 200", func(t *testing.T) {
		app := &Application{API: api.NewHandler(api.HandlerConfig{})}
		event := `{"requestContext":{"http":{"method":"GET","path":"/health"}},"rawPath":"/health","headers":{}}`
		resp, err := app.handleLambdaHTTPEvent(context.Background(), json.RawMessage(event))
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "degraded", topLevelHealthStatus(t, []byte(resp.Body)))
	})
}
