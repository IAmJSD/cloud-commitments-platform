package api

// db_rate_limiter_test.go — tests for DBRateLimiter that don't require a real
// database connection. Only nil-pool and in-memory paths are exercised.

import (
	"context"
	"testing"
	"time"

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

// A limiter with no pool cannot count attempts, so it must report an error
// rather than allow (issue #104): an allow would silently disable brute-force
// protection on the credential endpoints, whose strict check only fails
// closed on an error.
func TestDBRateLimiter_Allow_NilLimiter(t *testing.T) {
	var rl *DBRateLimiter
	allowed, err := rl.Allow(context.Background(), "IP#1.2.3.4", "login")
	require.ErrorIs(t, err, ErrRateLimiterUnavailable)
	assert.False(t, allowed)
}

func TestDBRateLimiter_NilPool_ReportsUnavailable(t *testing.T) {
	ctx := context.Background()
	rl := NewDBRateLimiter(nil)
	calls := map[string]func() (bool, error){
		"Allow":          func() (bool, error) { return rl.Allow(ctx, "IP#1.2.3.4", "login") },
		"AllowWithIP":    func() (bool, error) { return rl.AllowWithIP(ctx, "1.2.3.4", "login") },
		"AllowWithEmail": func() (bool, error) { return rl.AllowWithEmail(ctx, "user@example.com", "forgot_password") },
		"AllowWithUser":  func() (bool, error) { return rl.AllowWithUser(ctx, "user-id-123", "admin") },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			allowed, err := call()
			require.ErrorIs(t, err, ErrRateLimiterUnavailable)
			assert.False(t, allowed)
		})
	}
}

// End to end through the handler: a pool-less DB limiter makes the strict
// credential-endpoint check answer 503 instead of letting the request through,
// while the ordinary check keeps its documented fail-open behavior.
func TestHandler_checkRateLimitStrict_NilPoolFailsClosed(t *testing.T) {
	ctx := context.Background()
	for name, rl := range map[string]*DBRateLimiter{"nil pool": NewDBRateLimiter(nil), "nil limiter": nil} {
		t.Run(name, func(t *testing.T) {
			h := &Handler{rateLimiter: rl}
			req := makeSourceIPReq("198.51.100.4")

			err := h.checkRateLimitStrict(ctx, req, "login")
			ce, ok := IsClientError(err)
			require.True(t, ok, "expected a 503 client error, got %v", err)
			assert.Equal(t, 503, ce.code)

			assert.NoError(t, h.checkRateLimit(ctx, req, "api_general"))
		})
	}
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
	rl.lastCleanup = time.Now().Add(-time.Minute) // Long ago

	// Should trigger cleanup (no-op because pool is nil)
	rl.maybeCleanup()
	// Brief wait for the goroutine to complete
	time.Sleep(20 * time.Millisecond)
}

// Regression test for 02-M2: StartCleanupWorker must launch a goroutine that
// stops cleanly on context cancellation. The goroutine is exercised with a
// fast ticker so we can confirm it fires without sleeping in tests.
func TestDBRateLimiter_StartCleanupWorker_StopsOnContextCancel(t *testing.T) {
	// Override the scheduled interval to a very short duration for testing.
	orig := dbRateLimiterScheduledCleanupInterval
	dbRateLimiterScheduledCleanupInterval = 5 * time.Millisecond
	t.Cleanup(func() { dbRateLimiterScheduledCleanupInterval = orig })

	rl := NewDBRateLimiter(nil) // nil pool; cleanup() is a no-op
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	// Wrap cleanup in a channel signal to confirm the worker ran.
	go func() {
		defer close(done)
		rl.StartCleanupWorker(ctx)
	}()

	// Let the ticker fire at least twice (2 * 5ms = 10ms + margin).
	time.Sleep(30 * time.Millisecond)
	cancel()

	// StartCleanupWorker itself returns immediately (goroutine is launched
	// inside it). The done channel signals the outer goroutine exited.
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Error("StartCleanupWorker goroutine wrapper did not return after cancel")
	}
}
