//go:build integration
// +build integration

package api

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/providers/aws/recommendations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/require"
)

// getMigrationsPath resolves the migrations directory relative to this
// test file so the integration test works regardless of where `go
// test` is invoked from. Mirrors the helper in
// internal/config/store_postgres_test.go.
func getMigrationsPath() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "database", "postgres", "migrations")
}

// setupRICacheIntegration returns a store over a fully migrated database of
// the test's own on the package's shared Postgres server, so the test uses the
// same schema production does. The database is dropped when t ends.
func setupRICacheIntegration(ctx context.Context, t *testing.T) *config.PostgresStore {
	t.Helper()
	return config.NewPostgresStore(testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath()).DB)
}

// TestRIUtilizationCache_Integration_ColdReadFetchesAndPersists verifies
// that the first read with no existing row triggers the fetcher and
// persists the result in ri_utilization_cache.
func TestRIUtilizationCache_Integration_ColdReadFetchesAndPersists(t *testing.T) {
	ctx := context.Background()
	store := setupRICacheIntegration(ctx, t)

	cache := newRIUtilizationCache(store, false)
	fetched := []recommendations.RIUtilization{
		{ReservedInstanceID: "ri-integration-1", UtilizationPercent: 90.0, PurchasedHours: 720, TotalActualHours: 648},
	}
	var calls atomic.Int32
	fetch := func(ctx context.Context, lookbackDays int) ([]recommendations.RIUtilization, error) {
		calls.Add(1)
		return fetched, nil
	}

	got, err := cache.getOrFetch(ctx, usEast30, time.Hour, 2*time.Hour, fetch)
	require.NoError(t, err)
	require.Equal(t, int32(1), calls.Load())
	require.Len(t, got, 1)
	require.Equal(t, "ri-integration-1", got[0].ReservedInstanceID)

	// Postgres row must be present with the correct payload.
	entry, err := store.GetRIUtilizationCache(ctx, usEast30)
	require.NoError(t, err)
	require.NotNil(t, entry)
	require.Equal(t, riUtilAccountA, entry.CloudAccountID)
	require.Equal(t, "us-east-1", entry.Region)
	require.Equal(t, 30, entry.LookbackDays)
	var round []recommendations.RIUtilization
	require.NoError(t, json.Unmarshal(entry.Payload, &round))
	require.Equal(t, fetched, round)
}

// TestRIUtilizationCache_Integration_FreshHitNoFetch verifies that a
// second read within the soft TTL serves from Postgres without
// invoking the fetcher.
func TestRIUtilizationCache_Integration_FreshHitNoFetch(t *testing.T) {
	ctx := context.Background()
	store := setupRICacheIntegration(ctx, t)

	cache := newRIUtilizationCache(store, false)
	fetched := []recommendations.RIUtilization{{ReservedInstanceID: "ri-fresh-1"}}
	var calls atomic.Int32
	fetch := func(ctx context.Context, lookbackDays int) ([]recommendations.RIUtilization, error) {
		calls.Add(1)
		return fetched, nil
	}

	// Prime the cache.
	_, err := cache.getOrFetch(ctx, usEast30, time.Hour, 2*time.Hour, fetch)
	require.NoError(t, err)
	require.Equal(t, int32(1), calls.Load())

	// Second read: well within soft TTL.
	_, err = cache.getOrFetch(ctx, usEast30, time.Hour, 2*time.Hour, fetch)
	require.NoError(t, err)
	require.Equal(t, int32(1), calls.Load(), "fresh hit must not trigger a fetch")
}

// TestRIUtilizationCache_Integration_StaleNonLambdaServesStaleAndRefreshes
// verifies SWR end-to-end: stale row served synchronously, background
// refresh fires, Postgres row is updated. The fetcher blocks until the test
// releases it, so getOrFetch returning proves the stale read did not wait for
// the refresh, without an elapsed-time bound (issue #36).
func TestRIUtilizationCache_Integration_StaleNonLambdaServesStaleAndRefreshes(t *testing.T) {
	ctx := context.Background()
	store := setupRICacheIntegration(ctx, t)

	staleData := []recommendations.RIUtilization{{ReservedInstanceID: "ri-stale"}}
	freshData := []recommendations.RIUtilization{{ReservedInstanceID: "ri-fresh"}}

	// Seed a row whose age is between soft (50ms) and hard (1h).
	stalePayload, err := json.Marshal(staleData)
	require.NoError(t, err)
	require.NoError(t, store.UpsertRIUtilizationCache(ctx, usEast30, stalePayload, time.Now().Add(-200*time.Millisecond)))

	cache := newRIUtilizationCache(store, false) // non-Lambda
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFetch := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseFetch)
	//nolint:unparam // fetcher signature is fixed by getOrFetch; this refresh path never errors
	fetch := func(_ context.Context, _ int) ([]recommendations.RIUtilization, error) {
		<-release
		return freshData, nil
	}

	got := getOrFetchWithin(t, 10*time.Second, func() ([]recommendations.RIUtilization, error) {
		return cache.getOrFetch(ctx, usEast30, 50*time.Millisecond, time.Hour, fetch)
	}, "stale-read must return synchronously, not wait for background fetch")
	require.Len(t, got, 1)
	require.Equal(t, "ri-stale", got[0].ReservedInstanceID, "first call must return the stale copy")

	// Let the background refresh finish and wait for it to land the fresh data.
	releaseFetch()
	require.Eventually(t, func() bool {
		entry, err := store.GetRIUtilizationCache(ctx, usEast30)
		if err != nil || entry == nil {
			return false
		}
		var round []recommendations.RIUtilization
		_ = json.Unmarshal(entry.Payload, &round)
		return len(round) == 1 && round[0].ReservedInstanceID == "ri-fresh"
	}, 10*time.Second, 20*time.Millisecond, "background refresh did not update the ri_utilization_cache row")
}

// TestRIUtilizationCache_Integration_HardExpiredSyncRefetch verifies
// that rows beyond the hard TTL force a synchronous refetch even on
// non-Lambda: the caller blocks until fresh data arrives.
func TestRIUtilizationCache_Integration_HardExpiredSyncRefetch(t *testing.T) {
	ctx := context.Background()
	store := setupRICacheIntegration(ctx, t)

	staleData := []recommendations.RIUtilization{{ReservedInstanceID: "ri-stale"}}
	freshData := []recommendations.RIUtilization{{ReservedInstanceID: "ri-fresh"}}

	stalePayload, err := json.Marshal(staleData)
	require.NoError(t, err)
	// Age 2h exceeds both soft (15m) and hard (1h).
	require.NoError(t, store.UpsertRIUtilizationCache(ctx, usEast30, stalePayload, time.Now().Add(-2*time.Hour)))

	cache := newRIUtilizationCache(store, false)
	fetch := func(ctx context.Context, lookbackDays int) ([]recommendations.RIUtilization, error) {
		return freshData, nil
	}

	got, err := cache.getOrFetch(ctx, usEast30, 15*time.Minute, time.Hour, fetch)
	require.NoError(t, err)
	require.Equal(t, "ri-fresh", got[0].ReservedInstanceID, "hard-expired entry must force sync refetch")

	// Postgres row should have the fresh fetched_at timestamp, not
	// the 2h-old one we seeded.
	entry, err := store.GetRIUtilizationCache(ctx, usEast30)
	require.NoError(t, err)
	require.NotNil(t, entry)
	require.Less(t, time.Since(entry.FetchedAt), 5*time.Second, "fetched_at must be updated after sync refetch")
}

// TestRIUtilizationCache_Integration_AccountsInSameRegionKeepOwnRows pins
// issue #120 on real Postgres: two accounts write the same region and lookback
// concurrently and each reads back only its own payload. Before migration
// 000150 the table's key was (region, lookback_days), so the writers
// overwrote one another and both accounts read whichever row landed last.
func TestRIUtilizationCache_Integration_AccountsInSameRegionKeepOwnRows(t *testing.T) {
	ctx := context.Background()
	store := setupRICacheIntegration(ctx, t)

	keyA := usEast30
	keyB := config.RIUtilizationCacheKey{CloudAccountID: riUtilAccountB, Region: "us-east-1", LookbackDays: 30}
	unattributed := config.RIUtilizationCacheKey{Region: "us-east-1", LookbackDays: 30}
	payloadOf := func(id string) []byte {
		b, err := json.Marshal([]recommendations.RIUtilization{{ReservedInstanceID: id}})
		require.NoError(t, err)
		return b
	}
	writers := map[config.RIUtilizationCacheKey][]byte{
		keyA:         payloadOf("ri-account-a"),
		keyB:         payloadOf("ri-account-b"),
		unattributed: payloadOf("ri-ambient"),
	}

	// Several concurrent upserts per key, interleaved across keys, exercise
	// the ON CONFLICT path on the new three-column key.
	const perKey = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, perKey*len(writers))
	for key, payload := range writers {
		for range perKey {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs <- store.UpsertRIUtilizationCache(ctx, key, payload, time.Now())
			}()
		}
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	for key, payload := range writers {
		entry, err := store.GetRIUtilizationCache(ctx, key)
		require.NoError(t, err)
		require.NotNil(t, entry, "no row for %+v", key)
		require.Equal(t, key.CloudAccountID, entry.CloudAccountID)
		require.JSONEq(t, string(payload), string(entry.Payload), "row for %+v holds another account's payload", key)
	}

	missing, err := store.GetRIUtilizationCache(ctx, config.RIUtilizationCacheKey{
		CloudAccountID: "12000000-0000-4000-8000-00000000000c", Region: "us-east-1", LookbackDays: 30,
	})
	require.NoError(t, err)
	require.Nil(t, missing, "an account with no row of its own must miss, not read another account's row")
}
