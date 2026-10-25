package api

// db_rate_limiter_test.go — tests for DBRateLimiter that don't require a real
// database connection. Only nil-pool and in-memory paths are exercised.

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil/goroutines"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewDBRateLimiter(t *testing.T) {
	rl := NewDBRateLimiter(nil)
	assert.NotNil(t, rl)
	assert.NotNil(t, rl.limits)
}

func TestDBRateLimiter_SetLimit(t *testing.T) {
	rl := NewDBRateLimiter(nil)
	cfg := NewRateLimitConfig(10, 30)
	rl.SetLimit("test-endpoint", cfg)

	rl.limitsMu.RLock()
	stored := rl.limits["test-endpoint"]
	rl.limitsMu.RUnlock()

	assert.Equal(t, 10, stored.MaxAttempts)
}

func TestDBRateLimiter_SetLimit_NilLimitsMap(t *testing.T) {
	// Simulate a limiter whose limits map was nilled out
	rl := &DBRateLimiter{}
	cfg := NewRateLimitConfig(5, 60)
	rl.SetLimit("endpoint", cfg)

	assert.Equal(t, 5, rl.limits["endpoint"].MaxAttempts)
}

func TestDBRateLimiter_Allow_NilLimiter(t *testing.T) {
	var rl *DBRateLimiter
	// A nil receiver should be safe (fail-open)
	allowed, err := rl.Allow(context.Background(), "IP#1.2.3.4", "login")
	require.NoError(t, err)
	assert.True(t, allowed)
}

func TestDBRateLimiter_Allow_NilPool(t *testing.T) {
	rl := NewDBRateLimiter(nil)
	allowed, err := rl.Allow(context.Background(), "IP#1.2.3.4", "login")
	require.NoError(t, err)
	assert.True(t, allowed)
}

func TestDBRateLimiter_AllowWithIP_NilPool(t *testing.T) {
	rl := NewDBRateLimiter(nil)
	allowed, err := rl.AllowWithIP(context.Background(), "1.2.3.4", "login")
	require.NoError(t, err)
	assert.True(t, allowed)
}

func TestDBRateLimiter_AllowWithEmail_NilPool(t *testing.T) {
	rl := NewDBRateLimiter(nil)
	allowed, err := rl.AllowWithEmail(context.Background(), "user@example.com", "forgot_password")
	require.NoError(t, err)
	assert.True(t, allowed)
}

func TestDBRateLimiter_AllowWithUser_NilPool(t *testing.T) {
	rl := NewDBRateLimiter(nil)
	allowed, err := rl.AllowWithUser(context.Background(), "user-id-123", "admin")
	require.NoError(t, err)
	assert.True(t, allowed)
}

func TestDBRateLimiter_cleanup_NilPool(t *testing.T) {
	rl := &DBRateLimiter{pool: nil}
	// Should not panic
	rl.cleanup()
}

func TestDBRateLimiter_maybeCleanup_AlreadyRunning(t *testing.T) {
	rl := NewDBRateLimiter(nil)
	rl.cleanupRunning.Store(true)
	// Should return immediately without panicking
	rl.maybeCleanup()
	rl.cleanupRunning.Store(false)
}

func TestDBRateLimiter_maybeCleanup_TooSoon(t *testing.T) {
	rl := NewDBRateLimiter(nil)
	rl.cleanupInterval = time.Hour
	rl.lastCleanup = time.Now() // Just ran cleanup

	// Should return without triggering cleanup
	rl.maybeCleanup()
}

func TestDBRateLimiter_maybeCleanup_ReadyToCleanup(t *testing.T) {
	rl := NewDBRateLimiter(nil)
	rl.cleanupInterval = time.Millisecond
	before := time.Now().Add(-time.Minute) // Long ago
	rl.lastCleanup = before

	mark := goroutines.NewMark()
	// Should trigger cleanup (no-op because pool is nil)
	rl.maybeCleanup()

	// The trigger stamps lastCleanup before it starts the goroutine.
	rl.cleanupMu.Lock()
	stamped := rl.lastCleanup.After(before)
	rl.cleanupMu.Unlock()
	assert.True(t, stamped, "a due cleanup must record when it ran")

	// Wait for the cleanup goroutine to return and hand the slot back,
	// rather than sleeping a fixed 20ms (issue #36).
	require.Eventually(t, func() bool {
		return len(mark.StartedBy("api.(*DBRateLimiter).maybeCleanup")) == 0 && !rl.cleanupRunning.Load()
	}, 10*time.Second, time.Millisecond, "the cleanup goroutine did not finish and release cleanupRunning")
}

// Regression test for 02-M2: StartCleanupWorker must launch a goroutine that
// stops cleanly on context cancellation. The goroutine is found in the
// runtime's goroutine snapshot, so the test waits for its exit instead of
// sleeping (issue #36).
//
// With a nil pool, cleanup() is a no-op, so a tick is not observable here and
// the test no longer shortens dbRateLimiterScheduledCleanupInterval: the
// worker reads that variable on its own goroutine, and restoring it in
// t.Cleanup is a data race the race detector reports, since nothing orders
// the worker's read before the restore.
func TestDBRateLimiter_StartCleanupWorker_StopsOnContextCancel(t *testing.T) {
	rl := NewDBRateLimiter(nil) // nil pool; cleanup() is a no-op
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mark := goroutines.NewMark()
	rl.StartCleanupWorker(ctx)
	const worker = "api.(*DBRateLimiter).StartCleanupWorker"
	require.Len(t, mark.StartedBy(worker), 1, "StartCleanupWorker must run the worker on its own goroutine")

	cancel()
	require.Eventually(t, func() bool { return len(mark.StartedBy(worker)) == 0 },
		10*time.Second, time.Millisecond, "the cleanup worker did not exit after its context was canceled")
}
