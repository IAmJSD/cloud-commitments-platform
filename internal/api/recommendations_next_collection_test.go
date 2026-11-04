package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// freshnessJSON calls GET /api/recommendations/freshness on a handler built
// from schedule (as NewHandler would) at the fixed instant now, and returns
// the serialized next_collection_at value.
func freshnessJSON(t *testing.T, schedule string, now time.Time) string {
	t.Helper()
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	collected := now.Add(-3 * time.Hour)
	mockStore.On("GetRecommendationsFreshness", ctx).
		Return(&config.RecommendationsFreshness{LastCollectedAt: &collected}, nil)

	handler := &Handler{
		config:             mockStore,
		apiKey:             "test-key",
		now:                func() time.Time { return now },
		collectionSchedule: parseCollectionSchedule(schedule),
	}
	req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"x-api-key": "test-key"}}

	got, err := handler.getRecommendationsFreshness(ctx, req)
	require.NoError(t, err)
	body, err := json.Marshal(got)
	require.NoError(t, err)
	return string(mustField(t, body, "next_collection_at"))
}

// Issue #756: the first-run empty state needs the next collection time, so
// the freshness payload computes it from the deployment's schedule.
func TestFreshnessNextCollectionAt(t *testing.T) {
	now := time.Date(2026, 10, 27, 15, 4, 5, 0, time.UTC)

	t.Run("EventBridge cron schedule", func(t *testing.T) {
		assert.Equal(t, `"2026-10-28T02:00:00Z"`, freshnessJSON(t, "cron(0 2 * * ? *)", now))
	})
	t.Run("five-field cron schedule (GCP, AKS, Container Apps)", func(t *testing.T) {
		assert.Equal(t, `"2026-10-27T18:00:00Z"`, freshnessJSON(t, "0 */6 * * *", now))
	})

	// The key is always present so the frontend can rely on it.
	for name, schedule := range map[string]string{
		"unset (scheduled collection off)": "",
		"rate schedule (phase unknown)":    "rate(1 day)",
		"unparsable schedule":              "every day at 2",
		"no run within the horizon":        "cron(0 0 1 1 ? 2020)",
	} {
		t.Run(name+" is null", func(t *testing.T) {
			assert.Equal(t, "null", freshnessJSON(t, schedule, now))
		})
	}
}

func TestNewHandlerParsesRecommendationSchedule(t *testing.T) {
	now := time.Date(2026, 10, 27, 15, 4, 5, 0, time.UTC)

	h := NewHandler(HandlerConfig{RecommendationSchedule: "0 2 * * *"})
	next := h.NextCollectionAt(now)
	require.NotNil(t, next)
	assert.Equal(t, time.Date(2026, 10, 28, 2, 0, 0, 0, time.UTC), *next)

	assert.Nil(t, NewHandler(HandlerConfig{}).NextCollectionAt(now))
	assert.Nil(t, NewHandler(HandlerConfig{RecommendationSchedule: "cron(bad)"}).NextCollectionAt(now))
}
