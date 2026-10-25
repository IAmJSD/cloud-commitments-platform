//go:build integration

package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/aws/aws-lambda-go/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestConfigInfrastructureKeyHTTP(t *testing.T) {
	ctx := context.Background()
	pg := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")
	store := config.NewPostgresStore(pg.DB)
	authService := new(MockAuthService)
	handler := NewHandler(HandlerConfig{ConfigStore: store, AuthService: authService})
	// Preload the infrastructure key; this verifies authorization, not secret loading.
	handler.apiKey = uuid.NewString()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			http.Error(w, readErr.Error(), http.StatusBadRequest)
			return
		}
		resp, handleErr := handler.HandleRequest(r.Context(), &events.LambdaFunctionURLRequest{
			Body: string(body), Headers: map[string]string{"x-api-key": r.Header.Get("X-API-Key"), "content-type": "application/json"},
			RequestContext: events.LambdaFunctionURLRequestContext{HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: r.Method, Path: r.URL.Path}},
		})
		if handleErr != nil {
			http.Error(w, handleErr.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.WriteString(w, resp.Body)
	}))
	t.Cleanup(server.Close)
	account := uuid.NewString()
	require.NoError(t, store.CreateCloudAccount(ctx, &config.CloudAccount{ID: account, Name: account, Provider: "aws", ExternalID: "123456789012", Enabled: true}))
	initial, err := store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	initial.LadderingEnabled, initial.RIExchangeEnabled, initial.RIExchangeMode = false, false, "manual"
	require.NoError(t, store.SaveGlobalConfig(ctx, initial))
	for _, route := range []struct{ path, body string }{
		{"/api/ladder/configs", fmt.Sprintf(`{"cloud_account_id":%q,"provider":"aws","enabled":true,"mode":"email_approval","cadence":"daily","ramp_schedule":{"steps":[{"after_days":0,"fraction":1}]}}`, account)},
		{"/api/config", `{"laddering_enabled":true}`},
		{"/api/ri-exchange/config", `{"auto_exchange_enabled":true,"mode":"manual","utilization_threshold":50,"lookback_days":30}`},
	} {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPut, server.URL+route.path, strings.NewReader(route.body))
		require.NoError(t, reqErr)
		req.Header.Set("X-API-Key", handler.apiKey)
		resp, reqErr := server.Client().Do(req)
		require.NoError(t, reqErr)
		body, readErr := io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())
		require.NoError(t, readErr)
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s: %s", route.path, body)
	}
	ladders, err := store.GetLadderConfigs(ctx)
	require.NoError(t, err)
	require.Len(t, ladders, 1)
	require.True(t, ladders[0].Enabled)
	after, err := store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	require.True(t, after.LadderingEnabled)
	require.True(t, after.RIExchangeEnabled)
	require.Empty(t, authService.Calls)
	require.Empty(t, authService.UsageBookings())
}
