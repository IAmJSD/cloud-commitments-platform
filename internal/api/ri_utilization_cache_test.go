package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/providers/aws/recommendations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil/goroutines"
	"github.com/stretchr/testify/require"
)

// fakeRIUtilCacheStore is a minimal in-test implementation of
// riUtilizationCacheStore. Keyed by (region, lookbackDays); stores the
// raw JSON payload + fetched_at so the cache layer exercises the same
// marshaling path as the real Postgres store.
type fakeRIUtilCacheStore struct {
	getErr  error
	entries map[string]config.RIUtilizationCacheEntry
	mu      sync.Mutex
}

func newFakeRIUtilCacheStore() *fakeRIUtilCacheStore {
	return &fakeRIUtilCacheStore{entries: map[string]config.RIUtilizationCacheEntry{}}
}

func (f *fakeRIUtilCacheStore) key(region string, lookbackDays int) string {
	return region + "#" + time.Duration(lookbackDays).String()
}

func (f *fakeRIUtilCacheStore) GetRIUtilizationCache(ctx context.Context, region string, lookbackDays int) (*config.RIUtilizationCacheEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	e, ok := f.entries[f.key(region, lookbackDays)]
	if !ok {
		return nil, nil
	}
	return &e, nil
}

func (f *fakeRIUtilCacheStore) UpsertRIUtilizationCache(ctx context.Context, region string, lookbackDays int, payload []byte, fetchedAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[f.key(region, lookbackDays)] = config.RIUtilizationCacheEntry{
		Region:       region,
		LookbackDays: lookbackDays,
		Payload:      payload,
		FetchedAt:    fetchedAt,
	}
	return nil
}

// seedStale writes a cache row with fetchedAt set in the past so the
// caller can control exactly how "stale" the row is relative to soft
// / hard TTLs.
//
//nolint:unparam // test helper: region fixed by design across current callers
func (f *fakeRIUtilCacheStore) seedStale(t *testing.T, region string, lookbackDays int, data []recommendations.RIUtilization, age time.Duration) {
	t.Helper()
	payload, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("seedStale marshal: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[f.key(region, lookbackDays)] = config.RIUtilizationCacheEntry{
		Region:       region,
		LookbackDays: lookbackDays,
		Payload:      payload,
		FetchedAt:    time.Now().Add(-age),
	}
}

func (f *fakeRIUtilCacheStore) latest(region string, lookbackDays int) ([]recommendations.RIUtilization, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[f.key(region, lookbackDays)]
	if !ok {
		return nil, false
	}
	var out []recommendations.RIUtilization
	_ = json.Unmarshal(e.Payload, &out)
	return out, true
}

func TestRIUtilizationCache_HitReturnsCachedData(t *testing.T) {
	store := newFakeRIUtilCacheStore()
	c := newRIUtilizationCache(store, false)
	fetched := []recommendations.RIUtilization{{ReservedInstanceID: "ri-1", UtilizationPercent: 95.0}}
	var calls atomic.Int32

	fetch := func(ctx context.Context, lookbackDays int) ([]recommendations.RIUtilization, error) {
		calls.Add(1)
		return fetched, nil
	}

	got, err := c.getOrFetch(context.Background(), "us-east-1", 30, 10*time.Second, 30*time.Second, fetch)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if len(got) != 1 || got[0].ReservedInstanceID != "ri-1" {
		t.Fatalf("first call returned wrong data: %+v", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 fetch call; got %d", calls.Load())
	}

	// Second call within soft TTL: cache hit, fetcher NOT invoked.
	_, err = c.getOrFetch(context.Background(), "us-east-1", 30, 10*time.Second, 30*time.Second, fetch)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected still 1 fetch call after cache hit; got %d", calls.Load())
	}
}

// The fetcher blocks until the test releases it, so getOrFetch returning at
// all proves the stale copy was served without waiting for the refresh. That
// replaces an elapsed-time bound, and require.Eventually replaces the
// sleep-poll on the stored row (issue #36).
func TestRIUtilizationCache_StaleOnNonLambdaServesStaleAndKicksRefresh(t *testing.T) {
	store := newFakeRIUtilCacheStore()
	staleData := []recommendations.RIUtilization{{ReservedInstanceID: "ri-stale"}}
	freshData := []recommendations.RIUtilization{{ReservedInstanceID: "ri-fresh"}}
	// Seed a row whose age is between the soft TTL (50ms) and hard TTL (1h).
	store.seedStale(t, "us-east-1", 30, staleData, 100*time.Millisecond)

	c := newRIUtilizationCache(store, false) // non-Lambda

	fetchCalled := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFetch := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseFetch)
	//nolint:unparam // fetcher signature is fixed by getOrFetch; this refresh path never errors
	fetch := func(_ context.Context, _ int) ([]recommendations.RIUtilization, error) {
		fetchCalled <- struct{}{}
		<-release
		return freshData, nil
	}

	got := getOrFetchWithin(t, 10*time.Second, func() ([]recommendations.RIUtilization, error) {
		return c.getOrFetch(context.Background(), "us-east-1", 30, 50*time.Millisecond, time.Hour, fetch)
	}, "getOrFetch waited for the fetcher; SWR must return the stale copy synchronously")
	if len(got) != 1 || got[0].ReservedInstanceID != "ri-stale" {
		t.Fatalf("expected stale copy, got %+v", got)
	}

	select {
	case <-fetchCalled:
	case <-time.After(10 * time.Second):
		t.Fatalf("background fetch never fired")
	}
	releaseFetch()

	require.Eventually(t, func() bool {
		latest, ok := store.latest("us-east-1", 30)
		return ok && len(latest) == 1 && latest[0].ReservedInstanceID == "ri-fresh"
	}, 10*time.Second, time.Millisecond, "background refresh did not update the stored row")
}

// getOrFetchWithin runs call on another goroutine and fails the test with msg
// if it has not returned within limit. The limit only bounds a hang (a call
// stuck on a fetcher the test never releases); it is not a timing assertion.
func getOrFetchWithin(t *testing.T, limit time.Duration, call func() ([]recommendations.RIUtilization, error), msg string) []recommendations.RIUtilization {
	t.Helper()
	type result struct {
		err  error
		data []recommendations.RIUtilization
	}
	ret := make(chan result, 1)
	go func() {
		data, err := call()
		ret <- result{data: data, err: err}
	}()
	select {
	case r := <-ret:
		require.NoError(t, r.err)
		return r.data
	case <-time.After(limit):
		t.Fatal(msg)
		return nil
	}
}

func TestRIUtilizationCache_StaleOnLambdaBlocksForSyncRefetch(t *testing.T) {
	store := newFakeRIUtilCacheStore()
	staleData := []recommendations.RIUtilization{{ReservedInstanceID: "ri-stale"}}
	freshData := []recommendations.RIUtilization{{ReservedInstanceID: "ri-fresh"}}
	store.seedStale(t, "us-east-1", 30, staleData, 100*time.Millisecond)

	c := newRIUtilizationCache(store, true) // Lambda mode

	fetch := func(ctx context.Context, lookbackDays int) ([]recommendations.RIUtilization, error) {
		return freshData, nil
	}

	got, err := c.getOrFetch(context.Background(), "us-east-1", 30, 50*time.Millisecond, time.Hour, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Lambda must return the FRESH data synchronously (today's behavior).
	if len(got) != 1 || got[0].ReservedInstanceID != "ri-fresh" {
		t.Fatalf("Lambda mode should synchronously refetch; got %+v", got)
	}
}

func TestRIUtilizationCache_HardExpirySyncRefetch(t *testing.T) {
	store := newFakeRIUtilCacheStore()
	staleData := []recommendations.RIUtilization{{ReservedInstanceID: "ri-stale"}}
	freshData := []recommendations.RIUtilization{{ReservedInstanceID: "ri-fresh"}}
	// Age (2h) exceeds both soft (15m) and hard (1h).
	store.seedStale(t, "us-east-1", 30, staleData, 2*time.Hour)

	c := newRIUtilizationCache(store, false) // non-Lambda, but hard-expired

	fetch := func(ctx context.Context, lookbackDays int) ([]recommendations.RIUtilization, error) {
		return freshData, nil
	}

	got, err := c.getOrFetch(context.Background(), "us-east-1", 30, 15*time.Minute, time.Hour, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Past hard TTL: caller must block for fresh data, not get the stale copy.
	if len(got) != 1 || got[0].ReservedInstanceID != "ri-fresh" {
		t.Fatalf("hard-expired entry should force sync refetch; got %+v", got)
	}
}

func TestRIUtilizationCache_SingleflightCollapsesConcurrentRefreshes(t *testing.T) {
	store := newFakeRIUtilCacheStore()
	staleData := []recommendations.RIUtilization{{ReservedInstanceID: "ri-stale"}}
	freshData := []recommendations.RIUtilization{{ReservedInstanceID: "ri-fresh"}}
	store.seedStale(t, "us-east-1", 30, staleData, 100*time.Millisecond)

	c := newRIUtilizationCache(store, false)

	var calls atomic.Int32
	// Gate the fetcher so concurrent calls all race to enter the
	// refresh — singleflight should collapse them.
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFetch := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseFetch)
	//nolint:unparam // fetcher signature is fixed by newRIUtilizationCache; this refresh path never errors
	fetch := func(_ context.Context, _ int) ([]recommendations.RIUtilization, error) {
		calls.Add(1)
		<-release
		return freshData, nil
	}

	// Spawn 10 concurrent readers.
	const readers = 10
	mark := goroutines.NewMark()
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.getOrFetch(context.Background(), "us-east-1", 30, 50*time.Millisecond, time.Hour, fetch)
			if err != nil {
				t.Errorf("getOrFetch: %v", err)
			}
		}()
	}
	wg.Wait()

	// All 10 getOrFetch calls returned the stale copy, each kicking a
	// background refresh. Releasing the fetch before every refresh has
	// reached sf.Do would let a straggler open a second batch, so wait until
	// all 10 are accounted for: each one either called the fetcher, or is
	// parked in sf.Do on the in-flight call's WaitGroup as a duplicate. No
	// refresh can start a new fetch while the first is blocked, so once the
	// sum is 10 the fetch count is final (issue #36: this replaces a 250ms
	// head-start sleep that could still lose to a slow scheduler).
	require.Eventually(t, func() bool {
		return int(calls.Load())+len(sfDuplicatesWaiting(mark)) == readers
	}, 10*time.Second, time.Millisecond, "the background refreshes never all reached singleflight")
	if n := calls.Load(); n != 1 {
		t.Fatalf("expected exactly 1 fetch call under singleflight; got %d", n)
	}

	// Unblock the single in-flight fetch and wait for the collapsed batch to
	// store the fresh row and every refresh goroutine to return.
	releaseFetch()
	require.Eventually(t, func() bool {
		latest, ok := store.latest("us-east-1", 30)
		return ok && len(latest) == 1 && latest[0].ReservedInstanceID == "ri-fresh" &&
			len(mark.Started("riUtilizationCache).kickBackgroundRefresh")) == 0
	}, 10*time.Second, time.Millisecond, "the collapsed refresh did not store the fresh row and return")
	if n := calls.Load(); n != 1 {
		t.Fatalf("expected exactly 1 fetch call under singleflight after release; got %d", n)
	}
}

// sfDuplicatesWaiting returns the background refreshes started after mark
// that singleflight has registered as duplicates of an in-flight call: they
// are blocked on that call's WaitGroup inside Group.Do.
func sfDuplicatesWaiting(mark goroutines.Mark) []goroutines.Goroutine {
	return mark.Started("riUtilizationCache).kickBackgroundRefresh", "singleflight.(*Group).Do(", "sync.(*WaitGroup).Wait(")
}

func TestRIUtilizationCache_FetchErrorNotCached(t *testing.T) {
	store := newFakeRIUtilCacheStore()
	c := newRIUtilizationCache(store, true) // Lambda so we hit the sync path
	var calls atomic.Int32
	sentinel := errors.New("cost explorer down")
	fetch := func(ctx context.Context, lookbackDays int) ([]recommendations.RIUtilization, error) {
		calls.Add(1)
		return nil, sentinel
	}

	_, err := c.getOrFetch(context.Background(), "us-east-1", 30, time.Hour, time.Hour, fetch)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error; got %v", err)
	}
	// Retry should hit the fetcher again — we don't want to cache
	// failures and lock the dashboard out for the full TTL window.
	_, err = c.getOrFetch(context.Background(), "us-east-1", 30, time.Hour, time.Hour, fetch)
	if !errors.Is(err, sentinel) {
		t.Fatalf("second call: expected sentinel error; got %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 fetch calls (errors uncached); got %d", calls.Load())
	}
	if len(store.entries) != 0 {
		t.Fatalf("expected no rows written on fetch error; got %d", len(store.entries))
	}
}

func TestRIUtilizationCache_CorruptPayloadFallsThrough(t *testing.T) {
	store := newFakeRIUtilCacheStore()
	store.entries[store.key("us-east-1", 30)] = config.RIUtilizationCacheEntry{
		Region:       "us-east-1",
		LookbackDays: 30,
		Payload:      []byte("not-json"),
		FetchedAt:    time.Now(),
	}
	c := newRIUtilizationCache(store, true)
	fetched := []recommendations.RIUtilization{{ReservedInstanceID: "ri-fresh"}}
	var calls atomic.Int32
	fetch := func(ctx context.Context, lookbackDays int) ([]recommendations.RIUtilization, error) {
		calls.Add(1)
		return fetched, nil
	}

	got, err := c.getOrFetch(context.Background(), "us-east-1", 30, time.Hour, time.Hour, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected a re-fetch after corrupt payload; got %d fetch calls", calls.Load())
	}
	if len(got) != 1 || got[0].ReservedInstanceID != "ri-fresh" {
		t.Fatalf("got wrong data back: %+v", got)
	}
	// Corrupt row should have been overwritten.
	entry := store.entries[store.key("us-east-1", 30)]
	var round []recommendations.RIUtilization
	if err := json.Unmarshal(entry.Payload, &round); err != nil {
		t.Fatalf("stored payload still corrupt after upsert: %v", err)
	}
}

// TestHandler_getRIUtilizationCache_FollowsHandlerLambdaMode guards issue
// #110: the SWR gate must take the handler's configured Lambda mode
// (HandlerConfig.IsLambda), not a live read of AWS_LAMBDA_RUNTIME_API. Each
// case sets the environment to the opposite of the configured mode.
func TestHandler_getRIUtilizationCache_FollowsHandlerLambdaMode(t *testing.T) {
	for _, isLambda := range []bool{true, false} {
		t.Run(fmt.Sprintf("isLambda=%t", isLambda), func(t *testing.T) {
			env := ""
			if !isLambda {
				env = "127.0.0.1:9001"
			}
			t.Setenv("AWS_LAMBDA_RUNTIME_API", env)

			h := NewHandler(HandlerConfig{CORSAllowedOrigin: "https://islambda.example.test", IsLambda: isLambda})
			if got := h.IsLambda(); got != isLambda {
				t.Fatalf("Handler.IsLambda() = %t, want %t", got, isLambda)
			}
			if got := h.getRIUtilizationCache().isLambda; got != isLambda {
				t.Fatalf("RI utilization cache isLambda = %t, want %t (must follow HandlerConfig.IsLambda)", got, isLambda)
			}
		})
	}
}
