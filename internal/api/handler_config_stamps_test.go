package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #8: service config rows carry read-only created_at/updated_at stamps.

// TestGetConfig_ServiceRowsCarryStamps: GET /api/config returns each service
// row's stamps as RFC 3339 strings, and omits them for a config that has none.
func TestGetConfig_ServiceRowsCarryStamps(t *testing.T) {
	ctx := context.Background()
	created := time.Date(2026, 4, 27, 12, 34, 0, 0, time.UTC)
	updated := created.Add(26 * time.Hour)

	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockStore.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{EnabledProviders: []string{"aws"}}, nil)
	mockStore.On("ListServiceConfigs", mock.Anything).Return([]config.ServiceConfig{
		{Provider: "aws", Service: "ec2", CreatedAt: &created, UpdatedAt: &updated},
		{Provider: "aws", Service: "rds"},
	}, nil)
	handler := &Handler{config: mockStore, apiKey: "test-key"}
	handler.sourceIdentityOnce.Do(func() {}) // no STS lookup for the admin-only source identity

	resp, err := handler.HandleRequest(ctx, &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"X-API-Key": "test-key"},
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "GET", Path: "/api/config"},
		},
	})
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode)

	var body struct {
		Services []map[string]any `json:"services"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &body))
	require.Len(t, body.Services, 2)
	assert.Equal(t, "2026-04-27T12:34:00Z", body.Services[0]["created_at"])
	assert.Equal(t, "2026-04-28T14:34:00Z", body.Services[0]["updated_at"])
	assert.NotContains(t, body.Services[1], "created_at")
	assert.NotContains(t, body.Services[1], "updated_at")
}

// TestUpdateServiceConfig_IgnoresClientStamps: a PUT body that echoes the
// GET response's stamps (or forges them) must not reach the row the store
// writes, even for a first save where there is no stored row to merge onto.
func TestUpdateServiceConfig_IgnoresClientStamps(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("ValidateSession", ctx, "admin-token").Return(&Session{
		UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Email: "admin@example.com",
	}, nil)
	mockAuth.grantAdmin()

	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockStore.On("GetServiceConfig", ctx, "aws", "rds").Return(nil, nil)
	var saved *config.ServiceConfig
	mockStore.On("SaveServiceConfig", ctx, mock.AnythingOfType("*config.ServiceConfig")).
		Run(func(args mock.Arguments) { saved = args.Get(1).(*config.ServiceConfig) }).
		Return(nil)
	handler := &Handler{config: mockStore, auth: mockAuth}

	result, err := handler.updateServiceConfig(ctx, &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"Authorization": "Bearer admin-token"},
		Body: `{"enabled": true, "term": 3, "payment": "all-upfront", "coverage": 80,
			"created_at": "1999-01-01T00:00:00Z", "updated_at": "1999-01-01T00:00:00Z"}`,
	}, "aws/rds")
	require.NoError(t, err)
	assert.Equal(t, "updated", result.Status)
	require.NotNil(t, saved)
	assert.Nil(t, saved.CreatedAt, "a client-supplied created_at must not reach the store")
	assert.Nil(t, saved.UpdatedAt, "a client-supplied updated_at must not reach the store")
	assert.Equal(t, 80.0, saved.Coverage, "the editable fields still apply")
}
