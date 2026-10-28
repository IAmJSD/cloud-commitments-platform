package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sensitiveDialError has the shape of a pgx dial error: it names the database
// user, database and internal address. /health must never echo it (#111).
const sensitiveDialError = "failed to connect to `user=cudly_app database=cudly`: 10.0.3.7:5432 (db.internal): dial error"

// failingPingAuthStore is an auth store whose Ping fails with a pgx-shaped
// error.
type failingPingAuthStore struct{ *mockAuthStoreForHealth }

func (failingPingAuthStore) Ping(context.Context) error { return errors.New(sensitiveDialError) }

// TestHealth_DoesNotExposeRawErrors pins issue #111: /health is public, so a
// failed check reports a fixed message and a stable reason code, never the
// raw migration or store error.
func TestHealth_DoesNotExposeRawErrors(t *testing.T) {
	app := &Application{
		Version:  "test",
		Config:   &mockConfigStoreForHealth{},
		Auth:     auth.NewService(auth.ServiceConfig{Store: failingPingAuthStore{&mockAuthStoreForHealth{}}}),
		dbConfig: &database.Config{AutoMigrate: true},
	}
	app.recordMigrationResult(errors.New(`migration 000140_add_az_config.up.sql failed: ERROR: column "az_config" of relation "recommendations" already exists (SQLSTATE 42701)`))

	for _, path := range []string{"/health", "/ready"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			if path == "/health" {
				app.handleHealthCheck(w, req)
			} else {
				app.handleReadinessCheck(w, req)
			}

			body := w.Body.String()
			for _, leak := range []string{"10.0.3.7", "cudly_app", "dial error", "000140", "az_config", "SQLSTATE"} {
				assert.NotContains(t, body, leak, "/health body leaks internal error detail")
			}

			var health HealthStatus
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &health))
			assert.Equal(t, "degraded", health.Status)
			assert.Equal(t, CheckResult{Status: "unhealthy", Message: "Auth store ping failed", Reason: healthReasonAuthUnreachable}, health.Checks["auth_store"])
			assert.Equal(t, CheckResult{Status: "failed", Message: "Database migrations failed", Reason: healthReasonMigrationFailed}, health.Checks["migrations"])
			assert.True(t, strings.Contains(body, `"reason":"auth_store_unreachable"`), "reason code must be in the JSON body: %s", body)
		})
	}
}
