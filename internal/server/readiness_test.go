package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil"
)

// dbConfigForReadiness is the shape of a replica that has PostgreSQL configured
// and AutoMigrate on: its stores stay nil until initialization runs, which is
// exactly the state /ready must report as not-ready.
var dbConfigForReadiness = database.Config{Host: "unreachable", AutoMigrate: true}

// twoColdInstances wires two independent Application values the way two cold
// replicas of the same deployment are wired: neither has been initialized, and
// warming one leaves the other untouched. Reproduces the #488 report where a
// successful warm-up request on one instance did not establish readiness on the
// other.
func twoColdInstances() (warmed, cold *Application) {
	warmed = &Application{
		Version: "test",
		Config:  &mockConfigStoreForHealth{},
		Auth:    createHealthyAuthService(),
	}
	cold = &Application{
		Version:  "test",
		dbConfig: &dbConfigForReadiness,
	}
	return warmed, cold
}

// TestReadinessExcludesUninitializedReplica is the #488 regression test: a
// replica that never served user traffic must not be admitted. Pre-fix there
// was no /ready at all and /health answered 200, so the load balancer routed to
// it and the smoke test saw "degraded" from a replica nobody had initialized.
func TestReadinessExcludesUninitializedReplica(t *testing.T) {
	warmed, cold := twoColdInstances()

	// The warm-up path: one instance has completed initialization.
	warmedReq := httptest.NewRequestWithContext(context.Background(), "GET", "/ready", nil)
	warmedRes := httptest.NewRecorder()
	warmed.handleReadinessCheck(warmedRes, warmedReq)
	testutil.AssertEqual(t, http.StatusOK, warmedRes.Code)

	// The second instance is still cold. It must answer non-2xx even though a
	// sibling replica is perfectly ready: readiness is per-process.
	coldReq := httptest.NewRequestWithContext(context.Background(), "GET", "/ready", nil)
	coldRes := httptest.NewRecorder()
	cold.handleReadinessCheck(coldRes, coldReq)
	testutil.AssertEqual(t, http.StatusServiceUnavailable, coldRes.Code)

	var health HealthStatus
	testutil.AssertNoError(t, json.Unmarshal(coldRes.Body.Bytes(), &health))
	testutil.AssertEqual(t, "degraded", health.Status)
	// The body must still name the pending dependency: a 503 with an empty body
	// tells an operator nothing about which check to fix.
	testutil.AssertEqual(t, "pending", health.Checks["config_store"].Status)
	testutil.AssertEqual(t, "unhealthy", health.Checks["auth_store"].Status)
}

// TestReadinessPermanentInitializationFailure pins that a dependency that never
// becomes available keeps the replica out of rotation instead of admitting it
// with a 200. A cold replica that answers 503 forever is the correct outcome:
// the orchestrator's own failure threshold restarts it, and deployment
// verification fails within its budget.
func TestReadinessPermanentInitializationFailure(t *testing.T) {
	// Config and Auth stay nil: the state of a process whose ensureDB keeps
	// failing against an unreachable database.
	app := &Application{Version: "test", dbConfig: &dbConfigForReadiness}

	for range 3 {
		req := httptest.NewRequestWithContext(context.Background(), "GET", "/ready", nil)
		res := httptest.NewRecorder()
		app.handleReadinessCheck(res, req)
		testutil.AssertEqual(t, http.StatusServiceUnavailable, res.Code)

		var health HealthStatus
		testutil.AssertNoError(t, json.Unmarshal(res.Body.Bytes(), &health))
		testutil.AssertEqual(t, "degraded", health.Status)
		testutil.AssertEqual(t, "pending", health.Checks["config_store"].Status)
	}
}

// TestReadinessFailedMigrations pins the migration facet: a failed migration run
// is reported as "degraded" but the replica stays ready, because ensureDB never
// retries migrations and 503 would pull every replica after one transient error.
func TestReadinessFailedMigrations(t *testing.T) {
	app := &Application{
		Version:  "test",
		Config:   &mockConfigStoreForHealth{},
		Auth:     createHealthyAuthService(),
		dbConfig: &dbConfigForReadiness,
		// ensureDB finished: the stores above are the live ones.
		dbConnected: true,
	}
	app.recordMigrationResult(context.DeadlineExceeded)

	req := httptest.NewRequestWithContext(context.Background(), "GET", "/ready", nil)
	res := httptest.NewRecorder()
	app.handleReadinessCheck(res, req)
	testutil.AssertEqual(t, http.StatusOK, res.Code)

	var health HealthStatus
	testutil.AssertNoError(t, json.Unmarshal(res.Body.Bytes(), &health))
	testutil.AssertEqual(t, "failed", health.Checks["migrations"].Status)
	testutil.AssertEqual(t, "degraded", health.Status)
}

// TestReadinessPendingMigrations pins the pending case: AutoMigrate on, no
// migration attempt recorded yet, so this replica has not finished starting.
func TestReadinessPendingMigrations(t *testing.T) {
	app := &Application{
		Version:  "test",
		Config:   &mockConfigStoreForHealth{},
		Auth:     createHealthyAuthService(),
		dbConfig: &dbConfigForReadiness,
		// ensureDB finished: the stores above are the live ones.
		dbConnected: true,
	}

	req := httptest.NewRequestWithContext(context.Background(), "GET", "/ready", nil)
	res := httptest.NewRecorder()
	app.handleReadinessCheck(res, req)
	testutil.AssertEqual(t, http.StatusServiceUnavailable, res.Code)

	var health HealthStatus
	testutil.AssertNoError(t, json.Unmarshal(res.Body.Bytes(), &health))
	testutil.AssertEqual(t, "pending", health.Checks["migrations"].Status)
}

// TestReadinessDisabledMigrationsStayReady pins that turning AutoMigrate off is
// not a readiness failure: migrations run elsewhere, so this check reports
// "disabled" and a fully wired instance stays ready. Guards against making
// readiness stricter than the health contract it shares.
func TestReadinessDisabledMigrationsStayReady(t *testing.T) {
	// Local copy: mutating the shared dbConfigForReadiness fixture would leak
	// AutoMigrate=false into every other test in this file.
	noMigrate := database.Config{Host: "unreachable", AutoMigrate: false}
	app := &Application{
		Version:  "test",
		Config:   &mockConfigStoreForHealth{},
		Auth:     createHealthyAuthService(),
		dbConfig: &noMigrate,
		// ensureDB finished: the stores above are the live ones.
		dbConnected: true,
	}

	req := httptest.NewRequestWithContext(context.Background(), "GET", "/ready", nil)
	res := httptest.NewRecorder()
	app.handleReadinessCheck(res, req)
	testutil.AssertEqual(t, http.StatusOK, res.Code)
}

// TestLivenessStays200WhileDegraded pins the other half of the split: /health
// is the liveness contract and must keep answering 200 while degraded, so an
// orchestrator liveness probe never restarts a process that is up but whose
// dependencies are not connected. Pre-fix this was the only endpoint, which is
// why readiness could not be expressed at all.
func TestLivenessStays200WhileDegraded(t *testing.T) {
	app := &Application{Version: "test", dbConfig: &dbConfigForReadiness}

	req := httptest.NewRequestWithContext(context.Background(), "GET", "/health", nil)
	res := httptest.NewRecorder()
	app.handleHealthCheck(res, req)
	testutil.AssertEqual(t, http.StatusOK, res.Code)

	var health HealthStatus
	testutil.AssertNoError(t, json.Unmarshal(res.Body.Bytes(), &health))
	testutil.AssertEqual(t, "degraded", health.Status)
}

// TestReadinessWhileInitializationInProgress pins the non-blocking snapshot
// contract: while ensureDB holds dbMu, /ready must answer 503 with "pending"
// checks instead of blocking behind the initialization or racing on the store
// pointers it is rewriting.
func TestReadinessWhileInitializationInProgress(t *testing.T) {
	app := &Application{Version: "test", dbConfig: &dbConfigForReadiness}
	app.dbMu.Lock()
	defer app.dbMu.Unlock()

	req := httptest.NewRequestWithContext(context.Background(), "GET", "/ready", nil)
	res := httptest.NewRecorder()
	app.handleReadinessCheck(res, req)
	testutil.AssertEqual(t, http.StatusServiceUnavailable, res.Code)

	var health HealthStatus
	testutil.AssertNoError(t, json.Unmarshal(res.Body.Bytes(), &health))
	testutil.AssertEqual(t, "pending", health.Checks["config_store"].Status)
	testutil.AssertEqual(t, "pending", health.Checks["auth_store"].Status)
}

// TestReadyRouteRegistered pins that /ready is routed on the real server mux.
// A readiness handler that is never registered would leave the probe path
// serving the SPA index or 404, which an orchestrator reads as "not ready"
// forever; this test catches the wiring, not just the handler.
func TestReadyRouteRegistered(t *testing.T) {
	app := &Application{
		Version: "test",
		Config:  &mockConfigStoreForHealth{},
		Auth:    createHealthyAuthService(),
	}
	server := CreateHTTPServer(app, 0)

	rec := httptest.NewRecorder()
	server.Handler.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), "GET", "/ready", nil))
	testutil.AssertEqual(t, http.StatusOK, rec.Code)

	var health HealthStatus
	testutil.AssertNoError(t, json.Unmarshal(rec.Body.Bytes(), &health))
	testutil.AssertEqual(t, "healthy", health.Status)
}

// TestInitializeUntilReadyRetriesAfterTransientFailure pins the retry contract:
// a boot-time database outage must not strand the replica permanently unready.
// The first two attempts fail (a database still refusing connections), the
// third succeeds, and the loop must have made all three without any inbound
// request. Pre-fix there was no background initialization at all, so a cold
// replica stayed uninitialized until user traffic arrived.
func TestInitializeUntilReadyRetriesAfterTransientFailure(t *testing.T) {
	app := &Application{Version: "test", initRetryDelay: time.Millisecond}
	var mu sync.Mutex
	attempts := 0

	init := func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts < 3 {
			return errors.New("dial tcp: connection refused")
		}
		return nil
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		app.initializeUntilReady(context.Background(), init)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("initializeUntilReady did not return after init succeeded")
	}

	mu.Lock()
	defer mu.Unlock()
	testutil.AssertEqual(t, 3, attempts)
}

// TestInitializeUntilReadyStopsOnCancelledContext pins that shutdown stops the
// retry loop instead of leaving a goroutine connecting to a database that is
// going away. One attempt is always made, so an already-canceled context still
// tries once.
func TestInitializeUntilReadyStopsOnCancelledContext(t *testing.T) {
	app := &Application{Version: "test", initRetryDelay: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var mu sync.Mutex
	attempts := 0
	init := func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		return errors.New("dial tcp: connection refused")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		app.initializeUntilReady(ctx, init)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("initializeUntilReady did not return after the context was canceled")
	}

	mu.Lock()
	defer mu.Unlock()
	testutil.AssertEqual(t, 1, attempts)
}

// TestStartBackgroundInitSkipsWhenNoDatabase pins the Lambda/dev shape: with no
// dbConfig there is nothing to initialize, so no retry loop is started. Lambda
// must not pay for eager initialization on invocations that never touch the
// database.
func TestStartBackgroundInitSkipsWhenNoDatabase(t *testing.T) {
	app := &Application{Version: "test"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Returns synchronously; if it started a loop it would block here.
	done := app.startBackgroundInit(ctx)

	// Nothing to wait for, so shutdown must not stall on a channel nobody closes.
	select {
	case <-done:
	default:
		t.Fatal("done channel must already be closed when there is no database")
	}
}
