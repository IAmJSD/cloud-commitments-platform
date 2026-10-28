package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/mocks"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/scheduler"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
)

// markerStore is an in-memory stand-in for the recommendations_state
// collection marker. It mirrors the PostgresStore semantics of
// MarkCollectionStarted, ClearCollectionStarted (compare-and-clear) and
// ClaimCollectionInvocation (re-stamp when owned, free or, in Postgres,
// stale; this fake never goes stale), so the server-level tests can observe
// whether the marker is still set while a collect run is in progress. The
// real SQL is pinned against Postgres in
// internal/config/store_postgres_collection_invocation_test.go.
type markerStore struct {
	*mocks.MockConfigStore
	mu    sync.Mutex
	owner string
	set   bool
}

func newMarkerStore() *markerStore {
	return &markerStore{MockConfigStore: new(mocks.MockConfigStore)}
}

func (s *markerStore) MarkCollectionStarted(_ context.Context) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.set {
		return "", false, nil
	}
	s.owner, s.set = uuid.NewString(), true
	return s.owner, true, nil
}

func (s *markerStore) ClearCollectionStarted(_ context.Context, token string) error {
	if token == "" {
		return errors.New("owner token must not be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.set && s.owner == token {
		s.owner, s.set = "", false
	}
	return nil
}

func (s *markerStore) ClaimCollectionInvocation(_ context.Context, markerToken string) (string, bool, error) {
	if markerToken == "" {
		return "", false, errors.New("marker token must not be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.set && s.owner != markerToken {
		return "", false, nil
	}
	s.owner, s.set = uuid.NewString(), true
	return s.owner, true, nil
}

func (s *markerStore) markerSet() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.set
}

// sharedTaskLocker models pg_try_advisory_lock across concurrent
// invocations: the first TryAdvisoryLock for a lock ID wins until it is
// released, and every other attempt in the meantime is refused.
//
// afterAcquire, when set, runs once right after a successful acquisition and
// before TryAdvisoryLock returns, so a test can interleave another
// invocation between the winner taking the lock and its next step.
type sharedTaskLocker struct {
	afterAcquire func()
	held         map[int64]bool
	mu           sync.Mutex
}

func (l *sharedTaskLocker) TryAdvisoryLock(_ context.Context, lockID int64) (bool, error) {
	l.mu.Lock()
	if l.held == nil {
		l.held = map[int64]bool{}
	}
	if l.held[lockID] {
		l.mu.Unlock()
		return false, nil
	}
	l.held[lockID] = true
	hook := l.afterAcquire
	l.afterAcquire = nil
	l.mu.Unlock()
	if hook != nil {
		hook()
	}
	return true, nil
}

func (l *sharedTaskLocker) ReleaseAdvisoryLock(_ context.Context, lockID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.held, lockID)
}

// AdvisoryLockHeldFor reports a held lock as freshly taken, so a lock-skipped
// invocation takes the normal "already_running" path rather than the
// stuck-lock error (issue #394), which these tests do not exercise.
func (l *sharedTaskLocker) AdvisoryLockHeldFor(_ context.Context, lockID int64) (time.Duration, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return 0, l.held[lockID], nil
}

// blockingCollector stands in for scheduler.CollectRecommendations: it
// reports that collection started, blocks until released, then performs the
// scheduler's deferred token-scoped clear with the token it was handed.
type blockingCollector struct {
	store    *markerStore
	started  chan struct{}
	release  chan struct{}
	gotToken string
}

func newBlockingCollector(store *markerStore) *blockingCollector {
	return &blockingCollector{store: store, started: make(chan struct{}), release: make(chan struct{})}
}

func (c *blockingCollector) collect(ctx context.Context, ownerToken string) (*scheduler.CollectResult, error) {
	c.gotToken = ownerToken
	close(c.started)
	<-c.release
	if ownerToken != "" {
		_ = c.store.ClearCollectionStarted(ctx, ownerToken)
	}
	return &scheduler.CollectResult{}, nil
}

// TestHandleScheduledTask_DuplicateDeliveryKeepsWinnersMarker pins issue
// #84. Lambda's async invoke is at-least-once, so the refresh handler's
// self-invoke can be delivered twice with the same owner token. The first
// delivery wins the advisory lock and collects; the duplicate is
// lock-skipped. Before the fix the duplicate's skip-path release cleared the
// marker with that shared token while the winner was still collecting, so
// the in-progress banner dropped early and a refresh in the remaining window
// returned 202 without collecting. The winner now re-stamps the marker with
// its own invocation token once it holds the lock, so the duplicate's clear
// no longer matches.
func TestHandleScheduledTask_DuplicateDeliveryKeepsWinnersMarker(t *testing.T) {
	ctx := testutil.TestContext(t)
	store := newMarkerStore()
	collector := newBlockingCollector(store)
	app := &Application{
		Config:     store,
		Scheduler:  &testutil.MockScheduler{CollectRecommendationsFunc: collector.collect},
		Purchase:   &testutil.MockPurchaseManager{},
		TaskLocker: &sharedTaskLocker{},
	}

	markerToken, ok, err := store.MarkCollectionStarted(ctx)
	testutil.AssertNoError(t, err)
	testutil.AssertTrue(t, ok, "refresh must win the marker")
	params := ScheduledTaskParams{OwnerToken: markerToken}

	winnerDone := make(chan error, 1)
	go func() {
		_, runErr := app.HandleScheduledTask(ctx, TaskCollectRecommendations, params)
		winnerDone <- runErr
	}()
	<-collector.started

	// The duplicate delivery: same event, same token, lock held by the winner.
	result, err := app.HandleScheduledTask(ctx, TaskCollectRecommendations, params)
	testutil.AssertNoError(t, err)
	m, isMap := result.(map[string]string)
	testutil.AssertTrue(t, isMap && m["status"] == "skipped", "duplicate delivery must be lock-skipped")

	markerDuringCollect := store.markerSet()

	close(collector.release)
	testutil.AssertNoError(t, <-winnerDone)

	testutil.AssertTrue(t, markerDuringCollect,
		"the lock-skipped duplicate must not clear the marker while the winner is still collecting")
	testutil.AssertTrue(t, collector.gotToken != "" && collector.gotToken != markerToken,
		"the winner must collect under its own invocation token, not the shared marker token")
	testutil.AssertFalse(t, store.markerSet(),
		"the winner's deferred clear with its invocation token must release the marker")
}

// TestHandleScheduledTask_DuplicateClearsBeforeWinnerClaims covers the
// window between the winner taking the advisory lock and re-stamping the
// marker: a duplicate delivery that is lock-skipped inside that window still
// matches the shared marker token and clears the marker. The winner must
// then re-establish the marker under its own invocation token rather than
// collect with none, or the issue #84 symptom survives in that window.
func TestHandleScheduledTask_DuplicateClearsBeforeWinnerClaims(t *testing.T) {
	ctx := testutil.TestContext(t)
	store := newMarkerStore()
	collector := newBlockingCollector(store)
	locker := &sharedTaskLocker{}
	app := &Application{
		Config:     store,
		Scheduler:  &testutil.MockScheduler{CollectRecommendationsFunc: collector.collect},
		Purchase:   &testutil.MockPurchaseManager{},
		TaskLocker: locker,
	}

	markerToken, ok, err := store.MarkCollectionStarted(ctx)
	testutil.AssertNoError(t, err)
	testutil.AssertTrue(t, ok, "refresh must win the marker")
	params := ScheduledTaskParams{OwnerToken: markerToken}

	var duplicateErr error
	markerClearedInWindow := false
	locker.afterAcquire = func() {
		_, duplicateErr = app.HandleScheduledTask(ctx, TaskCollectRecommendations, params)
		markerClearedInWindow = !store.markerSet()
	}

	winnerDone := make(chan error, 1)
	go func() {
		_, runErr := app.HandleScheduledTask(ctx, TaskCollectRecommendations, params)
		winnerDone <- runErr
	}()
	<-collector.started
	markerDuringCollect := store.markerSet()
	close(collector.release)
	testutil.AssertNoError(t, <-winnerDone)
	testutil.AssertNoError(t, duplicateErr)

	testutil.AssertTrue(t, markerClearedInWindow, "precondition: the duplicate cleared the marker inside the window")
	testutil.AssertTrue(t, markerDuringCollect, "the lock winner must re-establish the marker before collecting")
	testutil.AssertTrue(t, collector.gotToken != "" && collector.gotToken != markerToken,
		"the winner must collect under its own invocation token")
	testutil.AssertFalse(t, store.markerSet(), "the winner's deferred clear must release the marker it re-established")
}

// TestHandleScheduledTask_SkippedBehindCronStillReleases pins the case the
// lock-skip release exists for (issue #261 follow-up), which the issue #84
// fix must keep: when the advisory lock is held by a tokenless cron run, a
// token-carrying refresh run that is skipped still owns the marker and must
// release it, or every refresh is rejected with 409 for five minutes.
func TestHandleScheduledTask_SkippedBehindCronStillReleases(t *testing.T) {
	ctx := testutil.TestContext(t)
	store := newMarkerStore()
	collector := newBlockingCollector(store)
	app := &Application{
		Config:     store,
		Scheduler:  &testutil.MockScheduler{CollectRecommendationsFunc: collector.collect},
		Purchase:   &testutil.MockPurchaseManager{},
		TaskLocker: &sharedTaskLocker{},
	}

	cronDone := make(chan error, 1)
	go func() {
		_, runErr := app.HandleScheduledTask(ctx, TaskCollectRecommendations, ScheduledTaskParams{})
		cronDone <- runErr
	}()
	<-collector.started

	markerToken, ok, err := store.MarkCollectionStarted(ctx)
	testutil.AssertNoError(t, err)
	testutil.AssertTrue(t, ok, "refresh must win the marker")
	_, err = app.HandleScheduledTask(ctx, TaskCollectRecommendations, ScheduledTaskParams{OwnerToken: markerToken})
	testutil.AssertNoError(t, err)

	markerAfterSkip := store.markerSet()
	close(collector.release)
	testutil.AssertNoError(t, <-cronDone)

	testutil.AssertFalse(t, markerAfterSkip, "a refresh run skipped behind cron must release its own marker")
	testutil.AssertEqual(t, "", collector.gotToken)
}

// TestHandleScheduledTask_ClaimInvocationOutcomes pins how the lock winner
// threads the ClaimCollectionInvocation result into the collect run.
func TestHandleScheduledTask_ClaimInvocationOutcomes(t *testing.T) {
	tests := []struct {
		claimErr   error
		name       string
		claimToken string
		wantToken  string
		claimOK    bool
	}{
		{
			name:       "claimed: collect under the invocation token",
			claimToken: "tok-invocation", claimOK: true,
			wantToken: "tok-invocation",
		},
		{
			// Another run holds a fresh marker: this run owns nothing, so it
			// collects tokenless and its deferred clear is skipped rather
			// than wiping the other run's marker.
			name:      "marker held by another run: collect tokenless",
			wantToken: "",
		},
		{
			// A store error degrades to the pre-#84 marker-scoped behavior
			// rather than failing a collection the lock already admitted.
			name:      "claim error: keep the marker token",
			claimErr:  errors.New("db connection lost"),
			wantToken: "tok-marker",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := testutil.TestContext(t)
			store := new(mocks.MockConfigStore)
			t.Cleanup(func() { store.AssertExpectations(t) })
			store.On("ClaimCollectionInvocation", mock.Anything, "tok-marker").
				Return(tt.claimToken, tt.claimOK, tt.claimErr).Once()

			gotToken := "unset"
			app := &Application{
				Config: store,
				Scheduler: &testutil.MockScheduler{
					CollectRecommendationsFunc: func(_ context.Context, ownerToken string) (*scheduler.CollectResult, error) {
						gotToken = ownerToken
						return &scheduler.CollectResult{}, nil
					},
				},
				Purchase:   &testutil.MockPurchaseManager{},
				TaskLocker: &mockTaskLocker{acquired: true},
			}

			_, err := app.HandleScheduledTask(ctx, TaskCollectRecommendations, ScheduledTaskParams{OwnerToken: "tok-marker"})
			testutil.AssertNoError(t, err)
			testutil.AssertEqual(t, tt.wantToken, gotToken)
		})
	}
}

// TestHandleScheduledTask_ClaimOnlyForTokenCarryingCollect pins that the
// re-stamp is confined to a token-carrying collect_recommendations run that
// won the lock: a tokenless (cron) collect owns no marker and another task
// type never touches it.
func TestHandleScheduledTask_ClaimOnlyForTokenCarryingCollect(t *testing.T) {
	cases := []struct {
		name     string
		taskType ScheduledTaskType
		params   ScheduledTaskParams
	}{
		{name: "tokenless collect", taskType: TaskCollectRecommendations},
		{name: "other task type with a stray token", taskType: TaskProcessScheduledPurchases, params: ScheduledTaskParams{OwnerToken: "tok-marker"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testutil.TestContext(t)
			store := new(mocks.MockConfigStore)
			app := &Application{
				Config:     store,
				Scheduler:  &testutil.MockScheduler{},
				Purchase:   &testutil.MockPurchaseManager{},
				TaskLocker: &mockTaskLocker{acquired: true},
			}
			_, err := app.HandleScheduledTask(ctx, tc.taskType, tc.params)
			testutil.AssertNoError(t, err)
			store.AssertNumberOfCalls(t, "ClaimCollectionInvocation", 0)
		})
	}
}
