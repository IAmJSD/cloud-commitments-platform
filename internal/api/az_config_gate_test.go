package api

import (
	"context"
	"fmt"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// A recommendations list below the az_config expand migrations answers the same
// 503 as the purchase paths, not a generic 500.
func TestGetRecommendations_MapsMissingAZConfigMigrationTo503(t *testing.T) {
	ctx := context.Background()

	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("HasAPIKeyPermissionAPI", ctx, "cudly-user-key", "view", "recommendations").
		Return("user-123", "key-abc", true, nil)
	// Account scoping resolves against the key's owning user.
	mockAuth.On("GetAllowedAccountsAPI", ctx, "user-123").
		Return([]string(nil), nil)
	// The unrestricted read probes whether it may kick a collection.
	mockAuth.On("HasAPIKeyPermissionForConstraintsAPI", ctx, "key-abc", "user-123", "view", "recommendations", mock.Anything).
		Return(true, nil)

	mockScheduler := new(MockScheduler)
	t.Cleanup(func() { mockScheduler.AssertExpectations(t) })
	mockScheduler.On("ListRecommendations", mock.Anything, mock.Anything).
		Return([]config.RecommendationRecord(nil), fmt.Errorf("suppression listing skipped: %w", config.ErrAZConfigMigrationNotReady))

	handler := &Handler{auth: mockAuth, scheduler: mockScheduler}
	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"x-api-key": "cudly-user-key"},
	}

	_, err := handler.getRecommendations(ctx, req, map[string]string{})
	ce, ok := IsClientError(err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, 503, ce.code)
	assert.Contains(t, ce.message, "000155 and 000156")
}
