package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil/goroutines"
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

	mark := goroutines.NewMark()
	recs, err := s.ListRecommendations(ctx, config.RecommendationFilter{})
	require.NoError(t, err)
	assert.Empty(t, recs)

	assertNoBackgroundCollect(t, mark, mockStore)
	mockStore.AssertNotCalled(t, "MarkCollectionStarted", mock.Anything)
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

	mark := goroutines.NewMark()
	_, err := s.ListRecommendations(ctx, config.RecommendationFilter{})
	require.NoError(t, err)

	assertNoBackgroundCollect(t, mark, mockStore)
	mockStore.AssertNotCalled(t, "MarkCollectionStarted", mock.Anything)
	assert.False(t, s.collecting.Load(), "an unmarked read must not claim the collection slot")
}

// assertNoBackgroundCollect checks that the read run since mark started no
// goroutine and made no GetGlobalConfig call, without a wall-clock wait
// (issue #36). A go statement puts its goroutine in the runtime's stack dump
// as soon as it executes, scheduled or not, and the goroutine stays there
// until it returns. So a collect the read wrongly spawned is either still
// listed by mark.Spawned, or it has finished, and then it went through
// CollectRecommendations, whose first store call is GetGlobalConfig (the mock
// records every call, expected or not). The goroutine snapshot is taken first
// so a collect that finishes in between is caught by the call log.
func assertNoBackgroundCollect(t *testing.T, mark goroutines.Mark, store *MockConfigStore) {
	t.Helper()
	for _, g := range mark.Spawned() {
		t.Errorf("the read started a background goroutine:\n%s", g.Stack)
	}
	store.AssertNotCalled(t, "GetGlobalConfig", mock.Anything)
}

// A marked read of a stale cache still kicks the background refresh: the
// allowed path keeps working.
func TestListRecommendations_MarkedStaleCache_KicksRefresh(t *testing.T) {
	ctx := WithBackgroundCollection(context.Background())
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })

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

// Pins the isLambda guard in maybeKickBackgroundRefresh.
//
// maybeKickBackgroundRefresh carries its own isLambda guard, separate from the
// one in ListRecommendations, so a future caller that skips the outer check
// still cannot start a refresh goroutine that freezes between Lambda
// invocations (issue #962). Calling it directly is the only way to reach that
// guard: through ListRecommendations the outer guard always returns first.
//
// The refresh's first store call (GetGlobalConfig inside
// CollectRecommendations) signals started, then blocks until the test hands
// it a value on release. The kick claims the collecting flag synchronously,
// so a wrongly kicked refresh still holds the flag when it is checked.
func TestMaybeKickBackgroundRefresh_LambdaGuard(t *testing.T) {
	stale := func() *config.RecommendationsFreshness {
		old := time.Now().Add(-72 * time.Hour)
		return &config.RecommendationsFreshness{LastCollectedAt: &old}
	}

	t.Run("lambda never kicks", func(t *testing.T) {
		mockStore, started, _ := blockingRefreshStore(t)
		s := &Scheduler{config: mockStore, isLambda: true}

		s.maybeKickBackgroundRefresh(stale(), time.Nanosecond)

		// The guard returns before the synchronous CAS, so a kick would
		// already hold the flag here; no wall-clock wait is needed.
		assert.False(t, s.collecting.Load(), "the collecting flag was claimed on Lambda")
		select {
		case <-started:
			t.Error("a background refresh reached the store on Lambda")
		default:
		}
		mockStore.AssertNotCalled(t, "GetGlobalConfig", mock.Anything)
	})

	// Positive control on the same harness: the refresh must start, and its
	// store call must still be blocked on release (flag held) until the test
	// releases it. If the harness stops blocking, nothing receives on release
	// and the send below times out, so the control fails every run.
	t.Run("non-lambda kicks (control)", func(t *testing.T) {
		mockStore, started, release := blockingRefreshStore(t)
		s := &Scheduler{config: mockStore, isLambda: false}

		s.maybeKickBackgroundRefresh(stale(), time.Nanosecond)

		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("expected a background refresh on a non-Lambda runtime")
		}
		assert.True(t, s.collecting.Load(), "the refresh must hold the collecting flag while its store call is blocked")
		select {
		case release <- struct{}{}:
		case <-time.After(2 * time.Second):
			t.Fatal("the refresh's store call did not wait for release; the harness no longer blocks it")
		}
		require.Eventually(t, func() bool { return !s.collecting.Load() }, 2*time.Second, time.Millisecond,
			"the refresh must release the collecting flag once its store call returns")
	})
}

// blockingRefreshStore returns a store whose GetGlobalConfig, the refresh's
// first store call, signals started and then waits for one value on release
// (or for the test to end) before failing, which ends the refresh.
func blockingRefreshStore(t *testing.T) (*MockConfigStore, <-chan struct{}, chan<- struct{}) {
	t.Helper()
	mockStore := new(MockConfigStore)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	mockStore.On("GetGlobalConfig", mock.Anything).
		Run(func(mock.Arguments) {
			started <- struct{}{}
			select {
			case <-release:
			case <-done:
			}
		}).
		Return(nil, errors.New("refresh started")).Maybe()
	return mockStore, started, release
}
