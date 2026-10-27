package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewInMemoryRateLimiter(t *testing.T) {
	rl := NewInMemoryRateLimiter()
	require.NotNil(t, rl)
	assert.NotNil(t, rl.attempts)
	assert.NotNil(t, rl.limits)
}

func TestInMemoryRateLimiter_SetLimit(t *testing.T) {
	rl := NewInMemoryRateLimiter()

	config := RateLimitConfig{
		MaxAttempts: 100,
		Window:      time.Hour,
	}
	rl.SetLimit("custom_endpoint", config)

	assert.Equal(t, config, rl.limits["custom_endpoint"])
}

func TestInMemoryRateLimiter_SetLimit_NilLimits(t *testing.T) {
	rl := &InMemoryRateLimiter{
		attempts: make(map[string]*inMemoryRateLimitEntry),
		limits:   nil,
	}

	config := RateLimitConfig{
		MaxAttempts: 50,
		Window:      time.Minute,
	}
	rl.SetLimit("test", config)

	assert.NotNil(t, rl.limits)
	assert.Equal(t, config, rl.limits["test"])
}

func TestInMemoryRateLimiter_Allow_NilReceiver(t *testing.T) {
	var rl *InMemoryRateLimiter
	ctx := context.Background()

	allowed, err := rl.Allow(ctx, "test-key", "api_general")
	require.NoError(t, err)
	assert.True(t, allowed)
}

func TestInMemoryRateLimiter_Allow_Success(t *testing.T) {
	rl := NewInMemoryRateLimiter()
	ctx := context.Background()

	// First request should be allowed
	allowed, err := rl.Allow(ctx, "test-key", "api_general")
	require.NoError(t, err)
	assert.True(t, allowed)
}

func TestInMemoryRateLimiter_Allow_RateLimited(t *testing.T) {
	rl := NewInMemoryRateLimiter()
	ctx := context.Background()

	// Set a very low limit for testing
	rl.SetLimit("test_endpoint", RateLimitConfig{
		MaxAttempts: 2,
		Window:      time.Hour,
	})

	// First two requests should be allowed
	allowed, err := rl.Allow(ctx, "test-key", "test_endpoint")
	require.NoError(t, err)
	assert.True(t, allowed)

	allowed, err = rl.Allow(ctx, "test-key", "test_endpoint")
	require.NoError(t, err)
	assert.True(t, allowed)

	// Third request should be blocked
	allowed, err = rl.Allow(ctx, "test-key", "test_endpoint")
	require.NoError(t, err)
	assert.False(t, allowed)
}

// fakeClock is a manually advanced clock for InMemoryRateLimiter tests, so
// window expiry is driven by Advance instead of a wall-clock sleep (#36).
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

// newInMemoryRateLimiterWithClock returns a limiter that reads clock instead
// of time.Now.
func newInMemoryRateLimiterWithClock(clock *fakeClock) *InMemoryRateLimiter {
	rl := NewInMemoryRateLimiter()
	rl.now = clock.Now
	return rl
}

// The window is an hour, so only the injected clock can expire it: a limiter
// that read time.Now would still block the third request.
func TestInMemoryRateLimiter_Allow_WindowExpiry(t *testing.T) {
	clock := newFakeClock()
	rl := newInMemoryRateLimiterWithClock(clock)
	ctx := context.Background()

	window := time.Hour
	rl.SetLimit("test_endpoint", RateLimitConfig{
		MaxAttempts: 1,
		Window:      window,
	})

	// First request should be allowed
	allowed, err := rl.Allow(ctx, "test-key", "test_endpoint")
	require.NoError(t, err)
	assert.True(t, allowed)

	// Second request should be blocked
	allowed, err = rl.Allow(ctx, "test-key", "test_endpoint")
	require.NoError(t, err)
	assert.False(t, allowed)

	// At exactly the reset time the window has not expired yet.
	clock.Advance(window)
	allowed, err = rl.Allow(ctx, "test-key", "test_endpoint")
	require.NoError(t, err)
	assert.False(t, allowed, "request at exactly the reset time must still be blocked")

	// One tick later the window has expired and a new one starts.
	clock.Advance(time.Nanosecond)
	allowed, err = rl.Allow(ctx, "test-key", "test_endpoint")
	require.NoError(t, err)
	assert.True(t, allowed, "request after the window expired must be allowed")

	// The new window counts from the expiry request.
	allowed, err = rl.Allow(ctx, "test-key", "test_endpoint")
	require.NoError(t, err)
	assert.False(t, allowed, "the new window must enforce the limit again")
}

func TestInMemoryRateLimiter_Allow_DifferentKeys(t *testing.T) {
	rl := NewInMemoryRateLimiter()
	ctx := context.Background()

	rl.SetLimit("test_endpoint", RateLimitConfig{
		MaxAttempts: 1,
		Window:      time.Hour,
	})

	// First key should be allowed
	allowed, err := rl.Allow(ctx, "key-1", "test_endpoint")
	require.NoError(t, err)
	assert.True(t, allowed)

	// Second key should also be allowed (different key)
	allowed, err = rl.Allow(ctx, "key-2", "test_endpoint")
	require.NoError(t, err)
	assert.True(t, allowed)

	// First key should be blocked
	allowed, err = rl.Allow(ctx, "key-1", "test_endpoint")
	require.NoError(t, err)
	assert.False(t, allowed)
}

func TestInMemoryRateLimiter_Allow_FallbackToGeneral(t *testing.T) {
	rl := NewInMemoryRateLimiter()
	ctx := context.Background()

	// Request with unknown endpoint should use api_general limits
	allowed, err := rl.Allow(ctx, "test-key", "unknown_endpoint")
	require.NoError(t, err)
	assert.True(t, allowed)
}

func TestInMemoryRateLimiter_AllowWithIP(t *testing.T) {
	rl := NewInMemoryRateLimiter()
	ctx := context.Background()

	allowed, err := rl.AllowWithIP(ctx, "192.168.1.1", "api_general")
	require.NoError(t, err)
	assert.True(t, allowed)
}

func TestInMemoryRateLimiter_AllowWithEmail(t *testing.T) {
	rl := NewInMemoryRateLimiter()
	ctx := context.Background()

	allowed, err := rl.AllowWithEmail(ctx, "user@example.com", "api_general")
	require.NoError(t, err)
	assert.True(t, allowed)
}

func TestInMemoryRateLimiter_AllowWithUser(t *testing.T) {
	rl := NewInMemoryRateLimiter()
	ctx := context.Background()

	allowed, err := rl.AllowWithUser(ctx, "user-123", "api_general")
	require.NoError(t, err)
	assert.True(t, allowed)
}

// At the entry cap, Allow first purges expired entries. Advancing the clock
// past the short window expires every short-window entry but not the
// long-window one, so the purge alone must bring the map well under the cap
// (the 80% oldest-first eviction would leave hundreds of entries) and must
// keep the live entry and its count.
func TestInMemoryRateLimiter_GarbageCollection(t *testing.T) {
	clock := newFakeClock()
	rl := newInMemoryRateLimiterWithClock(clock)
	ctx := context.Background()

	rl.SetLimit("short_endpoint", RateLimitConfig{MaxAttempts: 100, Window: time.Minute})
	rl.SetLimit("long_endpoint", RateLimitConfig{MaxAttempts: 1, Window: time.Hour})

	// The live entry is the oldest, so oldest-first eviction would drop it.
	allowed, err := rl.Allow(ctx, "live-key", "long_endpoint")
	require.NoError(t, err)
	require.True(t, allowed)
	for i := 1; i < inMemoryRateLimitMaxEntries; i++ {
		_, err = rl.Allow(ctx, fmt.Sprintf("key-%d", i), "short_endpoint")
		require.NoError(t, err)
	}
	rl.mu.Lock()
	require.Len(t, rl.attempts, inMemoryRateLimitMaxEntries)
	rl.mu.Unlock()

	// Expire the short-window entries, then trigger the cap check.
	clock.Advance(2 * time.Minute)
	_, err = rl.Allow(ctx, "trigger-gc", "short_endpoint")
	require.NoError(t, err)

	rl.mu.Lock()
	count := len(rl.attempts)
	rl.mu.Unlock()
	assert.Equal(t, 2, count, "expired entries must be purged, leaving the live entry and the new one")

	allowed, err = rl.Allow(ctx, "live-key", "long_endpoint")
	require.NoError(t, err)
	assert.False(t, allowed, "the live entry must survive the purge with its count intact")
}

func TestInMemoryRateLimiter_ConcurrentAccess(t *testing.T) {
	rl := NewInMemoryRateLimiter()
	ctx := context.Background()

	rl.SetLimit("test_endpoint", RateLimitConfig{
		MaxAttempts: 1000,
		Window:      time.Hour,
	})

	// Concurrent access test
	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func(id int) {
			for j := 0; j < 10; j++ {
				_, err := rl.Allow(ctx, string(rune('a'+id)), "test_endpoint")
				if err != nil {
					t.Errorf("unexpected error in goroutine: %v", err)
				}
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}
}

// Regression test for #411: setup_admin must have a dedicated strict rate-limit
// bucket (5/min/IP) and must NOT fall through to the permissive api_general
// config (300/min).
func TestGetDefaultRateLimits_SetupAdminHasDedicatedBucket(t *testing.T) {
	limits := getDefaultRateLimits()

	cfg, ok := limits["setup_admin"]
	if !ok {
		t.Fatal("setup_admin key missing from default rate limits (issue #411)")
	}

	// setup_admin should be far stricter than api_general (300/min)
	apiGeneral := limits["api_general"]
	if cfg.MaxAttempts >= apiGeneral.MaxAttempts {
		t.Errorf("setup_admin limit (%d) is not stricter than api_general (%d); regression of #411",
			cfg.MaxAttempts, apiGeneral.MaxAttempts)
	}

	// Verify the window is present
	if cfg.Window == 0 {
		t.Error("setup_admin rate limit has zero window duration")
	}

	// Verify the window matches the specification from issue #411 (5 per 15 minutes).
	assert.Equal(t, 15*time.Minute, cfg.Window,
		"setup_admin window must be 15 minutes per issue #411")
}

// Regression test for #1016: "register" must have a dedicated strict rate-limit
// bucket and must NOT fall through to the permissive api_general config (300/min).
// An attacker at 300/min can flood the DB with pending-registration rows and
// trigger a synchronous admin-notification email per request (inbox spam + DoS).
func TestGetDefaultRateLimits_RegisterHasDedicatedBucket(t *testing.T) {
	limits := getDefaultRateLimits()

	cfg, ok := limits["register"]
	if !ok {
		t.Fatal("register key missing from default rate limits (issue #1016): add it to getDefaultRateLimits()")
	}

	// Contract from #1016: 5 attempts per 15 minutes.
	assert.Equal(t, 5, cfg.MaxAttempts, "register max attempts must be 5 per issue #1016")
	assert.Equal(t, 15*time.Minute, cfg.Window, "register window must be 15 minutes per issue #1016")

	// register must be far stricter than api_general (300/min).
	apiGeneral := limits["api_general"]
	if cfg.MaxAttempts >= apiGeneral.MaxAttempts {
		t.Errorf("register limit (%d) is not stricter than api_general (%d); regression of #1016",
			cfg.MaxAttempts, apiGeneral.MaxAttempts)
	}

	// Window must be non-zero.
	if cfg.Window == 0 {
		t.Error("register rate limit has zero window duration")
	}
}

// Regression test for #1016: assertRateLimitKeysKnown must panic when an
// unregistered key is supplied, so a future caller typo is caught at startup
// rather than silently falling through to api_general.
func TestAssertRateLimitKeysKnown_PanicsOnUnknownKey(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("assertRateLimitKeysKnown did not panic for an unknown key")
		}
	}()
	assertRateLimitKeysKnown("this_key_does_not_exist_in_the_map")
}

// Regression test for #1016: assertRateLimitKeysKnown must not panic when all
// supplied keys are present in getDefaultRateLimits().
func TestAssertRateLimitKeysKnown_NoopForKnownKeys(t *testing.T) {
	// Should not panic.
	assertRateLimitKeysKnown("login", "setup_admin", "register", "approve_cancel_public")
}

// Regression test for #420: NewInMemoryRateLimiter must be ready at construction
// time (not after a slow lazy init) so Lambda cold-start first requests are protected.
func TestNewInMemoryRateLimiter_ReadyImmediately(t *testing.T) {
	rl := NewInMemoryRateLimiter()
	ctx := context.Background()

	// The limiter must be usable on the very first call without any further setup.
	// Previously, Lambda started with a nil rate limiter that allowed all requests
	// through until the DB connection was established (issue #420).
	allowed, err := rl.AllowWithIP(ctx, "10.0.0.1", "login")
	if err != nil {
		t.Fatalf("AllowWithIP returned unexpected error on first call: %v", err)
	}
	if !allowed {
		t.Fatal("first login attempt should be allowed by a fresh rate limiter")
	}

	// Exhaust the login limit (5 per 15 min)
	loginLimit := getDefaultRateLimits()["login"]
	for i := 1; i < loginLimit.MaxAttempts; i++ {
		_, _ = rl.AllowWithIP(ctx, "10.0.0.1", "login")
	}
	blocked, err := rl.AllowWithIP(ctx, "10.0.0.1", "login")
	if err != nil {
		t.Fatalf("unexpected error after exhausting limit: %v", err)
	}
	if blocked {
		t.Errorf("expected login to be blocked after %d attempts; rate limiter was not enforcing (regression of #420)", loginLimit.MaxAttempts)
	}
}

// Regression test for 02-M3: the in-memory rate limiter must not grow without
// bound even when entries are within their active window (not yet expired).
// Previously only expired entries were purged, allowing an attacker rotating
// source IPs to cause unbounded memory growth.
//
// Strategy: inject exactly cap+2 unique IPs with a long window. The (cap+1)-th
// call sees len==cap, triggers eviction to 80% of cap, then adds one entry.
// The (cap+2)-th call adds one more. Final count is ~(80%*cap)+2, which is
// strictly less than cap. A pre-fix run would reach cap+2.
func TestInMemoryRateLimiter_EntriesCapEnforced(t *testing.T) {
	rl := NewInMemoryRateLimiter()
	ctx := context.Background()

	// Fill to just above the cap so exactly one eviction cycle fires.
	total := inMemoryRateLimitMaxEntries + 2
	for i := 0; i < total; i++ {
		ip := fmt.Sprintf("10.%d.%d.%d", i/65536%256, i/256%256, i%256)
		_, _ = rl.AllowWithIP(ctx, ip, "login")
	}

	rl.mu.Lock()
	count := len(rl.attempts)
	rl.mu.Unlock()

	// After eviction the count must be strictly less than cap.
	if count >= inMemoryRateLimitMaxEntries {
		t.Errorf("entry count %d is >= cap %d after %d calls: eviction did not fire (02-M3 regression)",
			count, inMemoryRateLimitMaxEntries, total)
	}
}
