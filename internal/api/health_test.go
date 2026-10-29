package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// stubCredStore satisfies credentials.CredentialStore for health-check tests
// that just need the field to be non-nil.
type stubCredStore struct{ credentials.CredentialStore }

func TestHandler_GetHealth_AllHealthy(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockAuth := new(MockAuthService)

	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{}, nil)

	handler := &Handler{
		config:              mockStore,
		auth:                mockAuth,
		credStore:           &stubCredStore{},
		encryptionKeySource: credentials.EnvSecretARN, // simulate real key path
	}

	response, err := handler.GetHealth(ctx)
	require.NoError(t, err)

	assert.Equal(t, "healthy", response.Status)
	assert.NotNil(t, response.Timestamp)
	assert.Len(t, response.Checks, 3)
	assert.Equal(t, "healthy", response.Checks["config_store"].Status)
	assert.Equal(t, "healthy", response.Checks["auth_service"].Status)
	assert.Equal(t, "healthy", response.Checks["credential_store"].Status)
}

func TestHandler_checkCredentialStore_RealKey(t *testing.T) {
	h := &Handler{
		credStore:           &stubCredStore{},
		encryptionKeySource: credentials.EnvSecretName,
	}
	check := h.checkCredentialStore()
	assert.Equal(t, "healthy", check.Status)
	assert.Empty(t, check.Message)
}

func TestHandler_checkCredentialStore_DevKeyInUse(t *testing.T) {
	h := &Handler{
		credStore:           &stubCredStore{},
		encryptionKeySource: credentials.EnvAllowDev,
	}
	check := h.checkCredentialStore()
	assert.Equal(t, "degraded", check.Status)
	// /health is a public route: the body must not say which key is in use
	// (#15). The detail goes to the server log only.
	assert.Equal(t, "credential store degraded", check.Message)
}

// TestHandler_GetHealth_DevKeyNotDisclosed checks the public /health body
// never names the dev key or the env var that selected it (#15).
func TestHandler_GetHealth_DevKeyNotDisclosed(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{}, nil)

	handler := &Handler{
		config:              mockStore,
		auth:                new(MockAuthService),
		credStore:           &stubCredStore{},
		encryptionKeySource: credentials.EnvAllowDev,
	}

	response, err := handler.GetHealth(ctx)
	require.NoError(t, err)

	// Monitoring still sees the degraded state.
	assert.Equal(t, "degraded", response.Status)
	assert.Equal(t, "degraded", response.Checks["credential_store"].Status)

	body, err := json.Marshal(response)
	require.NoError(t, err)
	for _, leak := range []string{"dev_key", "dev key", credentials.EnvAllowDev, "CREDENTIAL_ENCRYPTION"} {
		assert.NotContains(t, strings.ToLower(string(body)), strings.ToLower(leak))
	}
}

func TestHandler_checkCredentialStore_NotConfigured(t *testing.T) {
	h := &Handler{credStore: nil}
	check := h.checkCredentialStore()
	assert.Equal(t, "unhealthy", check.Status)
	assert.Equal(t, "Credential store not initialized", check.Message)
}

func TestHandler_GetHealth_ConfigStoreUnhealthy(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockAuth := new(MockAuthService)

	mockStore.On("GetGlobalConfig", ctx).Return(nil, errors.New("database connection failed"))

	handler := &Handler{
		config: mockStore,
		auth:   mockAuth,
	}

	response, err := handler.GetHealth(ctx)
	require.NoError(t, err)

	assert.Equal(t, "degraded", response.Status)
	assert.Equal(t, "unhealthy", response.Checks["config_store"].Status)
	// Error detail must NOT appear in the public body (02-L4: info-leak fix).
	assert.Equal(t, "config store unavailable", response.Checks["config_store"].Message)
	assert.NotContains(t, response.Checks["config_store"].Message, "database connection failed")
	assert.Equal(t, "healthy", response.Checks["auth_service"].Status)
}

func TestHandler_GetHealth_AuthServiceUnhealthy(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })

	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{}, nil)

	handler := &Handler{
		config: mockStore,
		auth:   nil, // Auth service not configured
	}

	response, err := handler.GetHealth(ctx)
	require.NoError(t, err)

	assert.Equal(t, "degraded", response.Status)
	assert.Equal(t, "healthy", response.Checks["config_store"].Status)
	assert.Equal(t, "unhealthy", response.Checks["auth_service"].Status)
	assert.Contains(t, response.Checks["auth_service"].Message, "Auth service not initialized")
}

func TestHandler_GetHealth_BothUnhealthy(t *testing.T) {
	ctx := context.Background()

	handler := &Handler{
		config: nil, // Config store not configured
		auth:   nil, // Auth service not configured
	}

	response, err := handler.GetHealth(ctx)
	require.NoError(t, err)

	assert.Equal(t, "degraded", response.Status)
	assert.Equal(t, "unhealthy", response.Checks["config_store"].Status)
	assert.Contains(t, response.Checks["config_store"].Message, "Config store not initialized")
	assert.Equal(t, "unhealthy", response.Checks["auth_service"].Status)
	assert.Contains(t, response.Checks["auth_service"].Message, "Auth service not initialized")
}

func TestHandler_GetHealth_ConfigStoreNotInitialized(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)

	handler := &Handler{
		config: nil,
		auth:   mockAuth,
	}

	response, err := handler.GetHealth(ctx)
	require.NoError(t, err)

	assert.Equal(t, "degraded", response.Status)
	assert.Equal(t, "unhealthy", response.Checks["config_store"].Status)
	assert.Contains(t, response.Checks["config_store"].Message, "Config store not initialized")
}

func TestHandler_checkConfigStore_Healthy(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })

	mockStore.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{}, nil)

	handler := &Handler{config: mockStore}

	check := handler.checkConfigStore(ctx)

	assert.Equal(t, "healthy", check.Status)
	assert.Empty(t, check.Message)
}

func TestHandler_checkConfigStore_NotInitialized(t *testing.T) {
	ctx := context.Background()
	handler := &Handler{config: nil}

	check := handler.checkConfigStore(ctx)

	assert.Equal(t, "unhealthy", check.Status)
	assert.Equal(t, "Config store not initialized", check.Message)
}

func TestHandler_checkConfigStore_AccessError(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })

	mockStore.On("GetGlobalConfig", mock.Anything).Return(nil, errors.New("connection timeout"))

	handler := &Handler{config: mockStore}

	check := handler.checkConfigStore(ctx)

	assert.Equal(t, "unhealthy", check.Status)
	// Error detail must NOT appear in the public body (02-L4: info-leak fix).
	assert.Equal(t, "config store unavailable", check.Message)
	assert.NotContains(t, check.Message, "connection timeout")
}

func TestHandler_checkAuthService_Healthy(t *testing.T) {
	mockAuth := new(MockAuthService)

	handler := &Handler{auth: mockAuth}

	check := handler.checkAuthService()

	assert.Equal(t, "healthy", check.Status)
	assert.Empty(t, check.Message)
}

func TestHandler_checkAuthService_NotInitialized(t *testing.T) {
	handler := &Handler{auth: nil}

	check := handler.checkAuthService()

	assert.Equal(t, "unhealthy", check.Status)
	assert.Equal(t, "Auth service not initialized", check.Message)
}

func TestHealthResponse_Structure(t *testing.T) {
	response := &HealthResponse{
		Status: "healthy",
		Checks: map[string]HealthCheck{
			"test": {Status: "healthy", Message: ""},
		},
	}

	assert.Equal(t, "healthy", response.Status)
	assert.NotNil(t, response.Checks)
	assert.Len(t, response.Checks, 1)
}

func TestHealthCheck_Structure(t *testing.T) {
	check := HealthCheck{
		Status:  "unhealthy",
		Message: "Service unavailable",
	}

	assert.Equal(t, "unhealthy", check.Status)
	assert.Equal(t, "Service unavailable", check.Message)
}

// TestHandler_checkCredentialStore_DevKeyWarnsOnce checks the dev-key warning
// is logged once per process, not on every /health probe.
func TestHandler_checkCredentialStore_DevKeyWarnsOnce(t *testing.T) {
	devKeyWarnOnce = sync.Once{}
	t.Cleanup(func() { devKeyWarnOnce = sync.Once{} })
	buf := captureDefaultLog(t)

	h := &Handler{credStore: &stubCredStore{}, encryptionKeySource: credentials.EnvAllowDev}
	for i := 0; i < 3; i++ {
		assert.Equal(t, "degraded", h.checkCredentialStore().Status)
	}

	assert.Equal(t, 1, strings.Count(buf.String(), "insecure all-zero dev key"), buf.String())
}
