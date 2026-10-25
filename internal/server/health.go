package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
)

// HealthStatus represents the overall health of the application.
type HealthStatus struct {
	Timestamp time.Time              `json:"timestamp"`
	Checks    map[string]CheckResult `json:"checks"`
	Status    string                 `json:"status"`
	Version   string                 `json:"version"`
}

// CheckResult represents the result of a health check.
//
// /health and /ready are unauthenticated, so Message is always a fixed string
// and never carries a raw error: pgx and golang-migrate errors name the
// database host, user, migration file and schema objects (issue #111). A
// failed check sets Reason to a stable code instead, and the error itself goes
// to the server log only.
type CheckResult struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Stable reason codes a failed health check reports in CheckResult.Reason.
const (
	healthReasonMigrationFailed = "migration_failed"
	healthReasonDBUnreachable   = "db_unreachable"
	healthReasonAuthUnreachable = "auth_store_unreachable"
)

// handleHealthCheck returns the liveness status of the application.
//
// This endpoint is the LIVENESS contract: it always answers HTTP 200 so
// orchestrator liveness/startup probes never restart or kill a process that is
// up but whose dependencies are not connected yet. The verdict is in the JSON
// body ("healthy" / "degraded"). Traffic admission is a separate question
// answered by handleReadinessCheck (#488).
func (app *Application) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	writeHealthResponse(w, app.appConfig.CORSAllowedOrigin, http.StatusOK, app.collectHealth(ctx))
}

// handleReadinessCheck returns the READINESS status of the application.
//
// Readiness is the traffic-admission contract: a replica that has not completed
// required initialization answers 503, so the load balancer stops sending it
// requests. /health only reported "degraded" in the body while still answering
// 200, which admitted cold replicas to traffic and broke the deployment smoke
// gate (#488). Ready means required initialization is done; the body carries
// the same checks as /health so an operator sees which one is still pending.
// A failed migration run does not make the replica unready (see ready).
func (app *Application) handleReadinessCheck(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	health := app.collectHealth(ctx)
	status := http.StatusOK
	if !health.ready() {
		status = http.StatusServiceUnavailable
	}
	writeHealthResponse(w, app.appConfig.CORSAllowedOrigin, status, health)
}

// ready reports whether the replica may take traffic: the stores are connected
// and the first migration attempt has finished. A failed migration run stays
// "degraded" in the body but is still ready. ensureDB never retries migrations,
// so treating it as unready would pull every replica out of rotation after one
// transient error (e.g. a lock timeout) with nothing to bring them back.
func (h HealthStatus) ready() bool {
	for name, check := range h.Checks {
		if name == "migrations" {
			if check.Status == "pending" {
				return false
			}
			continue
		}
		if check.Status != "healthy" {
			return false
		}
	}
	return true
}

// collectHealth runs every readiness/health check and folds them into one
// overall status. Shared by /health and /ready so the two endpoints can never
// drift on what "initialized" means.
func (app *Application) collectHealth(ctx context.Context) HealthStatus {
	health := HealthStatus{
		Status:    "healthy",
		Version:   app.Version,
		Timestamp: time.Now(),
		Checks:    make(map[string]CheckResult),
	}

	// Check configuration and auth stores. ensureDB rewrites Config/Auth/DB
	// under dbMu and holds it for the full initialization, migrations
	// included; these endpoints must never block behind that. Snapshot the
	// pointers when the mutex is free, and while initialization holds it
	// report both stores as pending rather than race on half-written state.
	if app.dbMu.TryLock() {
		configStore, authService, db := app.Config, app.Auth, app.DB
		app.dbMu.Unlock()

		health.Checks["config_store"] = app.checkConfigStore(ctx, configStore, db)
		if health.Checks["config_store"].Status != "healthy" {
			health.Status = "degraded"
		}

		health.Checks["auth_store"] = app.checkAuthStore(ctx, authService)
		if health.Checks["auth_store"].Status != "healthy" {
			health.Status = "degraded"
		}
	} else {
		pending := CheckResult{Status: "pending", Message: "database initialization in progress"}
		health.Checks["config_store"] = pending
		health.Checks["auth_store"] = pending
		health.Status = "degraded"
	}

	// Check migrations. "disabled" and "healthy" are both acceptable; only
	// "pending" and "failed" flip the overall status to degraded.
	health.Checks["migrations"] = app.checkMigrations()
	switch health.Checks["migrations"].Status {
	case "failed", "pending":
		health.Status = "degraded"
	}

	return health
}

// writeHealthResponse encodes a health payload with the security headers and
// CORS both health endpoints share.
func writeHealthResponse(w http.ResponseWriter, corsOrigin string, status int, health HealthStatus) {
	setHealthResponseHeaders(w, corsOrigin)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(health); err != nil {
		// Body write failed after headers are sent; log for parity with
		// handleScheduledHTTP which already logs its encode error (04-L4).
		log.Printf("health: failed to encode response: %v", err)
	}
}

// checkMigrations reports the outcome of the most recent migration run.
// disabled = AutoMigrate is off; migrations happen elsewhere (e.g. CI)
// pending  = AutoMigrate is on but ensureDB hasn't completed yet
// failed   = last attempt returned an error OR timed out
// healthy  = last attempt completed without error.
func (app *Application) checkMigrations() CheckResult {
	// No dbConfig means the app isn't using PostgreSQL at all (DynamoDB or
	// test mode). AutoMigrate off means migrations are handled elsewhere
	// (e.g. a dedicated CI deploy step). Either way, "disabled" correctly
	// reports that this health facet is not applicable — the overall
	// status stays healthy.
	if app.dbConfig == nil || !app.dbConfig.AutoMigrate {
		return CheckResult{Status: "disabled", Message: "AutoMigrate is off"}
	}
	err, finishedAt := app.snapshotMigrationState()
	switch {
	case finishedAt.IsZero():
		return CheckResult{Status: "pending", Message: "migrations have not run yet"}
	case err != nil:
		// ensureDB logged the error when the run failed.
		return CheckResult{Status: "failed", Message: "Database migrations failed", Reason: healthReasonMigrationFailed}
	default:
		return CheckResult{
			Status:  "healthy",
			Message: fmt.Sprintf("last run %s ago", time.Since(finishedAt).Truncate(time.Second)),
		}
	}
}

// checkConfigStore checks the health of the configuration store. The store and
// connection come in as a snapshot taken under dbMu by collectHealth, so this
// function never reads pointers ensureDB may be rewriting.
func (app *Application) checkConfigStore(ctx context.Context, configStore config.StoreInterface, db *database.Connection) CheckResult {
	// Check if config store exists
	if configStore == nil {
		// If using PostgreSQL with lazy initialization, DB might not be connected yet
		if app.dbConfig != nil {
			return CheckResult{
				Status:  "pending",
				Message: "Database connection pending (lazy initialization)",
			}
		}
		return CheckResult{
			Status:  "unhealthy",
			Message: "Config store not initialized",
		}
	}

	// If using PostgreSQL, check database connection health
	if db != nil {
		if err := db.HealthCheck(ctx); err != nil {
			log.Printf("health: database health check failed: %v", err)
			return CheckResult{
				Status:  "unhealthy",
				Message: "Database health check failed",
				Reason:  healthReasonDBUnreachable,
			}
		}
	}

	return CheckResult{
		Status: "healthy",
	}
}

// setHealthResponseHeaders adds security headers and CORS to the health endpoint response.
// These match the headers set by internal/api/handler.go for API responses, ensuring
// consistent security posture when hitting the container directly without a CDN.
func setHealthResponseHeaders(w http.ResponseWriter, corsOrigin string) {
	w.Header().Set("Content-Type", "application/json")

	// Security headers
	w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-XSS-Protection", "1; mode=block")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	w.Header().Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")

	// CORS headers
	if corsOrigin != "" {
		w.Header().Set("Access-Control-Allow-Origin", corsOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, Authorization, X-Authorization, X-CSRF-Token")
	}
}

// checkAuthStore checks the health of the auth store. The service comes in as
// a snapshot taken under dbMu by collectHealth, so this function never reads a
// pointer ensureDB may be rewriting.
func (app *Application) checkAuthStore(ctx context.Context, authService *auth.Service) CheckResult {
	if authService == nil {
		return CheckResult{
			Status:  "unhealthy",
			Message: "Auth service not initialized",
		}
	}

	// Ping the database to verify connection is healthy
	if err := authService.Ping(ctx); err != nil {
		log.Printf("health: auth store ping failed: %v", err)
		return CheckResult{
			Status:  "unhealthy",
			Message: "Auth store ping failed",
			Reason:  healthReasonAuthUnreachable,
		}
	}

	return CheckResult{
		Status: "healthy",
	}
}
