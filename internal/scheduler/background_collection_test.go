package scheduler

import (
	"context"
	"errors"
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

// maybeKickBackgroundRefresh carries its own isLambda guard, separate from the
// one in ListRecommendations, so a future caller that skips the outer check
// still cannot start a refresh goroutine that freezes between Lambda
// invocations (issue #962). Calling it directly is the only way to reach that
// guard: through ListRecommendations the outer guard always returns first.
//
// The refresh's first store call (GetGlobalConfig inside
// CollectRecommendations) blocks until the test ends, so on a wrongly kicked
// refresh the collecting flag, which the kick sets synchronously, is still
// held when it is checked. The non-Lambda case is the positive control: the
// same harness must see the refresh start.
func TestMaybeKickBackgroundRefresh_LambdaGuard(t *testing.T) {
	for _, tc := range []struct {
		name     string
		isLambda bool
		wantKick bool
	}{
		{name: "lambda never kicks", isLambda: true, wantKick: false},
		{name: "non-lambda kicks (control)", isLambda: false, wantKick: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mockStore := new(MockConfigStore)
			started := make(chan struct{}, 1)
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			mockStore.On("GetGlobalConfig", mock.Anything).
				Run(func(mock.Arguments) {
					started <- struct{}{}
					<-release
				}).
				Return(nil, errors.New("refresh started")).Maybe()

			s := &Scheduler{config: mockStore, isLambda: tc.isLambda}
			old := time.Now().Add(-72 * time.Hour)
			s.maybeKickBackgroundRefresh(&config.RecommendationsFreshness{LastCollectedAt: &old}, time.Nanosecond)

			assert.Equal(t, tc.wantKick, s.collecting.Load(), "collecting flag after the kick")
			select {
			case <-started:
				assert.True(t, tc.wantKick, "a background refresh started on Lambda")
			case <-time.After(200 * time.Millisecond):
				assert.False(t, tc.wantKick, "expected a background refresh on a non-Lambda runtime")
			}
		})
	}
}
