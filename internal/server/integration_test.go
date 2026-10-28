//go:build integration
// +build integration

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/scheduler"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil"
)

// TestServerIntegration is an example integration test using testcontainers
// Run with: go test -tags=integration ./internal/server/...
func TestServerIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := testutil.TestContext(t)

	// An empty database of its own on the package's shared PostgreSQL server:
	// nothing here reads the schema yet, only the connection settings.
	pg := testhelpers.RequireEmptyDatabase(ctx, t)
	dbCfg := pg.Config

	// Set environment variables for database connection
	for key, value := range map[string]string{
		"DB_HOST":     dbCfg.Host,
		"DB_PORT":     strconv.Itoa(dbCfg.Port),
		"DB_NAME":     dbCfg.Database,
		"DB_USER":     dbCfg.User,
		"DB_PASSWORD": dbCfg.Password,
		"DB_SSL_MODE": dbCfg.SSLMode,
	} {
		testutil.SetEnv(t, key, value)
	}

	// TODO: When database package is ready, uncomment this:
	// app, err := NewApplication(ctx)
	// if err != nil {
	// 	t.Fatalf("Failed to create application: %v", err)
	// }
	// defer app.Close()

	// For now, just verify the database is reachable
	if err := pg.DB.Pool().Ping(ctx); err != nil {
		t.Fatalf("Failed to reach postgres database: %v", err)
	}
	t.Logf("PostgreSQL database %s running at %s:%d", dbCfg.Database, dbCfg.Host, dbCfg.Port)
}

// TestHealthCheckIntegration tests the health check endpoint.
func TestHealthCheckIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Create minimal application for health check
	app := &Application{
		Version: "test",
	}

	// Create HTTP test server
	req := httptest.NewRequestWithContext(context.Background(), "GET", "/health", nil)
	w := httptest.NewRecorder()

	// Call health check handler
	app.handleHealthCheck(w, req)

	// Verify response
	testutil.AssertEqual(t, http.StatusOK, w.Code)

	// Verify response is JSON
	contentType := w.Header().Get("Content-Type")
	testutil.AssertContains(t, contentType, "application/json")

	t.Logf("Health check response: %s", w.Body.String())
}

// TestScheduledTaskIntegration tests scheduled task execution.
func TestScheduledTaskIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := testutil.TestContext(t)

	// Create application with mock dependencies. The task runs synchronously,
	// so counting calls replaces the old 100ms "simulate actual work" sleep,
	// which slowed the test without checking anything (issue #36).
	var collects atomic.Int32
	mockScheduler := &testutil.MockScheduler{
		CollectRecommendationsFunc: func(ctx context.Context, ownerToken string) (*scheduler.CollectResult, error) {
			collects.Add(1)
			return &scheduler.CollectResult{}, nil
		},
	}

	app := &Application{
		Scheduler: mockScheduler,
	}

	// Test collect_recommendations task
	result, err := app.HandleScheduledTask(ctx, TaskCollectRecommendations, ScheduledTaskParams{})
	testutil.AssertNoError(t, err)
	testutil.AssertTrue(t, result != nil, "Result should not be nil")
	testutil.AssertEqual(t, int32(1), collects.Load())

	t.Logf("Scheduled task completed successfully")
}

// TestApplicationLifecycle tests full application startup and shutdown.
func TestApplicationLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Set up test environment
	testutil.SetEnv(t, "VERSION", "integration-test")
	testutil.SetEnv(t, "CONFIG_TABLE", "test-config")
	testutil.SetEnv(t, "PLANS_TABLE", "test-plans")
	testutil.SetEnv(t, "HISTORY_TABLE", "test-history")
	testutil.SetEnv(t, "USERS_TABLE", "test-users")
	testutil.SetEnv(t, "GROUPS_TABLE", "test-groups")
	testutil.SetEnv(t, "SESSIONS_TABLE", "test-sessions")

	// TODO: When NewApplication is fully implemented, test it:
	// app, err := NewApplication(ctx)
	// if err != nil {
	// 	t.Fatalf("Failed to create application: %v", err)
	// }
	// defer app.Close()

	// For now, create minimal app
	app := &Application{
		Version: "integration-test",
	}

	// Verify version is set
	testutil.AssertEqual(t, "integration-test", app.Version)

	// Test cleanup
	err := app.Close()
	testutil.AssertNoError(t, err)

	t.Logf("Application lifecycle test completed")
}
