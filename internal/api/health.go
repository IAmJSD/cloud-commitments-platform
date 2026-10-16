package api

import (
	"context"
	"sync"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/credentials"
)

// HealthResponse represents the health check response.
type HealthResponse struct {
	Timestamp time.Time              `json:"timestamp"`
	Checks    map[string]HealthCheck `json:"checks"`
	Status    string                 `json:"status"`
}

// HealthCheck represents a single health check result.
type HealthCheck struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// GetHealth performs comprehensive health checks.
func (h *Handler) GetHealth(ctx context.Context) (*HealthResponse, error) {
	response := &HealthResponse{
		Status:    "healthy",
		Timestamp: time.Now(),
		Checks:    make(map[string]HealthCheck),
	}

	// Check configuration store (includes database connection)
	configCheck := h.checkConfigStore(ctx)
	response.Checks["config_store"] = configCheck
	if configCheck.Status != "healthy" {
		response.Status = "degraded"
	}

	// Check auth service (includes database connection)
	authCheck := h.checkAuthService()
	response.Checks["auth_service"] = authCheck
	if authCheck.Status != "healthy" {
		response.Status = "degraded"
	}

	// Check credential store (encryption key validity + dev-key detection)
	credCheck := h.checkCredentialStore()
	response.Checks["credential_store"] = credCheck
	if credCheck.Status != "healthy" {
		response.Status = "degraded"
	}

	return response, nil
}

// checkConfigStore checks if the configuration store is accessible.
func (h *Handler) checkConfigStore(ctx context.Context) HealthCheck {
	if h.config == nil {
		return HealthCheck{
			Status:  "unhealthy",
			Message: "Config store not initialized",
		}
	}

	// Try to access config to verify database connectivity
	_, err := h.config.GetGlobalConfig(ctx)
	if err != nil {
		// Log the detailed error server-side; return a generic message to avoid
		// leaking connection details (host, db name, SQLSTATE) on this public route.
		logging.Errorf("health: config store check failed: %v", err)
		return HealthCheck{
			Status:  "unhealthy",
			Message: "config store unavailable",
		}
	}

	return HealthCheck{
		Status: "healthy",
	}
}

// checkCredentialStore reports the state of the credential encryption key.
// Returns one of three logical states via Status + Message:
//
//   - "healthy" / "ok": real key loaded from a Secrets Manager / Vault.
//   - "degraded" / "credential store degraded": CREDENTIAL_ENCRYPTION_ALLOW_DEV_KEY=1
//     was set, so the all-zero key is in use. Acceptable for local dev only;
//     monitoring should alert on this state in any deployed environment.
//     /health is a public route, so the body stays generic and the cause
//     (dev key in use) goes to the server log only, like checkConfigStore (#15).
//   - "unhealthy" / "not configured": no credStore was wired in (config error).
//
// NOTE: "ok" only confirms the key is valid (LoadKey succeeded + startup guard
// passed); it does NOT guarantee that all DB rows have been re-keyed. Detect
// pre-rekey state by alerting on application-level decrypt-failure ERROR logs.
// devKeyWarnOnce limits the dev-key warning to once per process: /health is
// polled every few seconds by load balancers, and the degraded status in the
// response already keeps monitoring alerting.
var devKeyWarnOnce sync.Once

func (h *Handler) checkCredentialStore() HealthCheck {
	if h.credStore == nil {
		return HealthCheck{
			Status:  "unhealthy",
			Message: "Credential store not initialized",
		}
	}
	if h.encryptionKeySource == credentials.EnvAllowDev {
		devKeyWarnOnce.Do(func() {
			logging.Warnf("health: credential store is using the insecure all-zero dev key (%s=1)", credentials.EnvAllowDev)
		})
		return HealthCheck{
			Status:  "degraded",
			Message: "credential store degraded",
		}
	}
	return HealthCheck{Status: "healthy"}
}

// checkAuthService checks if the auth service is accessible.
func (h *Handler) checkAuthService() HealthCheck {
	if h.auth == nil {
		return HealthCheck{
			Status:  "unhealthy",
			Message: "Auth service not initialized",
		}
	}

	// Just verify the service exists and is configured
	// We don't want to create test users or sessions in health checks
	return HealthCheck{
		Status: "healthy",
	}
}
