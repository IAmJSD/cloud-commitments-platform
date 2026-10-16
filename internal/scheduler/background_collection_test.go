package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #627: the cold-start and stale-cache kicks in ListRecommendations
// collect across every registered account, and the cold-start kick takes the
// cluster-wide in-flight marker. They run only when the caller's ctx carries
// WithBackgroundCollection, which the API sets for unrestricted readers only.

// An unmarked read of an empty cache on a non-Lambda runtime is a pure cache
// read: no marker and no collection.
func TestListRecommendations_UnmarkedColdStart_NeverMarksOrCollects(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })

	mockStore.On("GetRecommendationsFreshness", ctx).
		Return(&config.RecommendationsFreshness{LastCollectedAt: nil}, nil)
	mockStore.On("ListStoredRecommendations", ctx, mock.Anything).
		Return([]config.RecommendationRecord{}, nil)

	s := &Scheduler{config: mockStore, isLambda: false}

	recs, err := s.ListRecommendations(ctx, config.RecommendationFilter{})
	require.NoError(t, err)
	assert.Empty(t, recs)

	// A wrongly spawned collect would hit GetGlobalConfig first.
	time.Sleep(20 * time.Millisecond)
	mockStore.AssertNotCalled(t, "MarkCollectionStarted", mock.Anything)
	mockStore.AssertNotCalled(t, "GetGlobalConfig", mock.Anything)
	assert.False(t, s.collecting.Load(), "an unmarked read must not claim the collection slot")
}

// An unmarked read of a stale cache on a non-Lambda runtime does not kick the
// background refresh, and does not even resolve the stale TTL.
func TestListRecommendations_UnmarkedStaleCache_NeverRefreshes(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })

	old := time.Now().Add(-72 * time.Hour)
	mockStore.On("GetRecommendationsFreshness", ctx).
		Return(&config.RecommendationsFreshness{LastCollectedAt: &old}, nil)
	mockStore.On("ListStoredRecommendations", ctx, mock.Anything).
		Return([]config.RecommendationRecord{}, nil)

	s := &Scheduler{config: mockStore, isLambda: false, cacheTTL: time.Nanosecond}

	_, err := s.ListRecommendations(ctx, config.RecommendationFilter{})
	require.NoError(t, err)

	time.Sleep(20 * time.Millisecond)
	mockStore.AssertNotCalled(t, "GetGlobalConfig", mock.Anything)
	mockStore.AssertNotCalled(t, "MarkCollectionStarted", mock.Anything)
	assert.False(t, s.collecting.Load(), "an unmarked read must not claim the collection slot")
}

// A marked read of a stale cache still kicks the background refresh: the
// allowed path keeps working.
func TestListRecommendations_MarkedStaleCache_KicksRefresh(t *testing.T) {
	ctx := WithBackgroundCollection(context.Background())
	mockStore := new(MockConfigStore)

	old := time.Now().Add(-72 * time.Hour)
	collected := make(chan struct{})
	mockStore.On("GetRecommendationsFreshness", ctx).
		Return(&config.RecommendationsFreshness{LastCollectedAt: &old}, nil)
	mockStore.On("ListStoredRecommendations", ctx, mock.Anything).
		Return([]config.RecommendationRecord{}, nil)
	// resolveEffectiveCacheTTL, synchronously on the caller's ctx: 1h TTL.
	mockStore.On("GetGlobalConfig", ctx).
		Return(&config.GlobalConfig{RecommendationsCacheStaleHours: 1}, nil).Once()
	// CollectRecommendations, on the detached background ctx. No enabled
	// providers, so no cloud call is made.
	mockStore.On("GetGlobalConfig", mock.MatchedBy(func(c context.Context) bool { return c != ctx })).
		Run(func(mock.Arguments) { close(collected) }).
		Return(&config.GlobalConfig{EnabledProviders: []string{}}, nil).Once()
	mockStore.On("UpsertRecommendations", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	s := &Scheduler{config: mockStore, isLambda: false}

	_, err := s.ListRecommendations(ctx, config.RecommendationFilter{})
	require.NoError(t, err)

	select {
	case <-collected:
	case <-time.After(2 * time.Second):
		t.Fatal("a marked read of a stale cache must kick the background refresh")
	}
	require.Eventually(t, func() bool { return !s.collecting.Load() }, 2*time.Second, 5*time.Millisecond)
	mockStore.AssertNotCalled(t, "MarkCollectionStarted", mock.Anything)
}

// The scheduled collection (the cron's collect_recommendations task) calls
// CollectRecommendations directly on a ctx that never carries the marker; the
// gate is on ListRecommendations only, so the scheduled path still collects.
func TestCollectRecommendations_ScheduledPathIgnoresReadGate(t *testing.T) {
	ctx := context.Background()
	require.False(t, BackgroundCollectionAllowed(ctx))
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })

	mockStore.On("GetGlobalConfig", ctx).
		Return(&config.GlobalConfig{EnabledProviders: []string{}}, nil).Once()

	s := &Scheduler{config: mockStore, email: new(MockEmailSender)}

	result, err := s.CollectRecommendations(ctx, "")
	require.NoError(t, err)
	require.NotNil(t, result)
}

func TestBackgroundCollectionAllowed(t *testing.T) {
	assert.False(t, BackgroundCollectionAllowed(context.Background()))
	assert.True(t, BackgroundCollectionAllowed(WithBackgroundCollection(context.Background())))
	// The mark survives derivation, e.g. the HTTP layer's request timeout.
	derived, cancel := context.WithTimeout(WithBackgroundCollection(context.Background()), time.Minute)
	defer cancel()
	assert.True(t, BackgroundCollectionAllowed(derived))
}
