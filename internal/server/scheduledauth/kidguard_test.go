package scheduledauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// signedFor signs a token that passes every claim check, so the only
// thing that can fail is the signature/key lookup.
func signedFor(t *testing.T, key *testKey, now time.Time) string {
	t.Helper()
	return "Bearer " + signToken(t, key, baseClaims(now,
		testSchedulerSubject, "https://api.example.com", "https://accounts.example.com"))
}

// swapJWKS replaces the document newJWKSServer serves.
func swapJWKS(t *testing.T, srv *jwksServer, body []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/swap", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build swap request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("swap: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("swap: status %d: %s", resp.StatusCode, b)
	}
}

func negativeKidCount(v *Validator) int {
	v.kids.negative.mu.Lock()
	defer v.kids.negative.mu.Unlock()
	return len(v.kids.negative.entries)
}

// Issue #75: once a JWKS fetched after a token arrived has shown its kid
// is unknown, further tokens with that kid must be rejected without
// another JWKS fetch. Before the fix every such token cost one fetch
// (21 for this loop).
func TestValidate_OIDC_RepeatedUnknownKidSkipsRefetch(t *testing.T) {
	keyA := newTestKey(t, "kid-A")
	keyX := newTestKey(t, "kid-X") // never published
	srv := newJWKSServer(t, jwks(keyA))
	v := newOIDCValidator(t, srv.URL)
	t0 := time.Now()
	v.now = func() time.Time { return t0 }

	if err := v.Validate(context.Background(), signedFor(t, keyA, t0)); err != nil {
		t.Fatalf("warm-up token: %v", err)
	}
	before := srv.hits.Load()

	bad := signedFor(t, keyX, t0)
	for i := 0; i < 20; i++ {
		if err := v.Validate(context.Background(), bad); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("unknown-kid token %d: expected ErrUnauthorized, got %v", i, err)
		}
	}
	if got := srv.hits.Load() - before; got != 1 {
		t.Fatalf("20 tokens with the same unknown kid made %d JWKS fetches; want 1", got)
	}

	// Legitimate traffic is unaffected.
	if err := v.Validate(context.Background(), signedFor(t, keyA, t0)); err != nil {
		t.Fatalf("valid token after unknown-kid spam: %v", err)
	}
}

// A negative entry is bounded by negativeKidTTL: a kid that is published
// after it was cached is accepted once the entry expires.
func TestValidate_OIDC_NegativeKidExpiresAfterTTL(t *testing.T) {
	keyA := newTestKey(t, "kid-A")
	keyB := newTestKey(t, "kid-B")
	srv := newJWKSServer(t, jwks(keyA))
	v := newOIDCValidator(t, srv.URL)
	t0 := time.Now()
	current := t0
	v.now = func() time.Time { return current }

	tokB := signedFor(t, keyB, t0)
	if err := v.Validate(context.Background(), tokB); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("kid-B before publication: expected ErrUnauthorized, got %v", err)
	}
	swapJWKS(t, srv, jwks(keyA, keyB))

	current = t0.Add(negativeKidTTL - time.Second)
	before := srv.hits.Load()
	if err := v.Validate(context.Background(), tokB); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("kid-B inside the negative TTL: expected ErrUnauthorized, got %v", err)
	}
	if got := srv.hits.Load() - before; got != 0 {
		t.Fatalf("negatively cached kid made %d JWKS fetches inside its TTL; want 0", got)
	}

	current = t0.Add(negativeKidTTL)
	if err := v.Validate(context.Background(), tokB); err != nil {
		t.Fatalf("kid-B once the negative entry expired: %v", err)
	}
}

// A negative entry never hides a key the most recent JWKS fetch already
// contains: once any refresh has picked up the rotated key, tokens signed
// by it are accepted at once, even inside the entry's TTL.
func TestValidate_OIDC_NegativeKidBypassedOncePublished(t *testing.T) {
	keyA := newTestKey(t, "kid-A")
	keyB := newTestKey(t, "kid-B")
	keyY := newTestKey(t, "kid-Y") // never published
	srv := newJWKSServer(t, jwks(keyA))
	v := newOIDCValidator(t, srv.URL)
	t0 := time.Now()
	v.now = func() time.Time { return t0 }

	tokB := signedFor(t, keyB, t0)
	if err := v.Validate(context.Background(), tokB); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("kid-B before publication: expected ErrUnauthorized, got %v", err)
	}
	swapJWKS(t, srv, jwks(keyA, keyB))

	// Any other refresh (here a different unknown kid) fetches the rotated
	// document that now carries kid-B.
	if err := v.Validate(context.Background(), signedFor(t, keyY, t0)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("kid-Y: expected ErrUnauthorized, got %v", err)
	}
	if err := v.Validate(context.Background(), tokB); err != nil {
		t.Fatalf("kid-B after a refresh published it, inside its negative TTL: %v", err)
	}
}

// Forged tokens that reuse a published kid must not trigger a refetch
// each, and must not lock out genuine tokens carrying that kid.
func TestValidate_OIDC_ForgedPublishedKidDoesNotBlockLegit(t *testing.T) {
	keyA := newTestKey(t, "kid-A")
	imposter := newTestKey(t, "kid-A") // same kid, different key
	srv := newJWKSServer(t, jwks(keyA))
	v := newOIDCValidator(t, srv.URL)
	t0 := time.Now()
	v.now = func() time.Time { return t0 }

	legit := signedFor(t, keyA, t0)
	if err := v.Validate(context.Background(), legit); err != nil {
		t.Fatalf("warm-up token: %v", err)
	}
	before := srv.hits.Load()
	forged := signedFor(t, imposter, t0)
	for i := 0; i < 10; i++ {
		if err := v.Validate(context.Background(), forged); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("forged token %d: expected ErrUnauthorized, got %v", i, err)
		}
	}
	if got := srv.hits.Load() - before; got != 1 {
		t.Fatalf("10 forged tokens with a published kid made %d JWKS fetches; want 1", got)
	}
	for i := 0; i < 3; i++ {
		if err := v.Validate(context.Background(), legit); err != nil {
			t.Fatalf("legit token %d after forged spam: %v", i, err)
		}
	}
}

// A kid is only cached when a JWKS fetch that completed successfully
// after the token arrived lacked it. If the refresh fails, nothing is
// cached and the next token tries again (fail closed, no poisoning).
func TestValidate_OIDC_FailedRefreshDoesNotCacheKid(t *testing.T) {
	keyA := newTestKey(t, "kid-A")
	keyX := newTestKey(t, "kid-X")
	var (
		hits    atomic.Int64
		failing atomic.Bool
	)
	body := jwks(keyA)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if failing.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(ts.Close)
	v := newOIDCValidator(t, ts.URL)
	t0 := time.Now()
	v.now = func() time.Time { return t0 }

	if err := v.Validate(context.Background(), signedFor(t, keyA, t0)); err != nil {
		t.Fatalf("warm-up token: %v", err)
	}
	failing.Store(true)
	bad := signedFor(t, keyX, t0)
	for i := 0; i < 3; i++ {
		// go-oidc's keysFromRemote marks the in-flight fetch done before it
		// clears r.inflight (jwks.go:196-234), so a back-to-back Validate can
		// reuse the finished fetch without a new request. Retry until one
		// fetch happens; every call must still fail closed.
		before := hits.Load()
		deadline := time.Now().Add(2 * time.Second)
		for hits.Load() == before {
			if err := v.Validate(context.Background(), bad); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("token %d: expected ErrUnauthorized, got %v", i, err)
			}
			if time.Now().After(deadline) {
				t.Fatalf("token %d: no JWKS fetch within 2s although no refresh ever confirmed the kid unknown", i)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	if n := negativeKidCount(v); n != 0 {
		t.Fatalf("negative cache holds %d entries after failed refreshes; want 0", n)
	}
}

// Concurrent valid and unknown-kid traffic: run with -race. Valid tokens
// must always pass, bad ones always fail, and JWKS fetches stay bounded.
func TestValidate_OIDC_NegativeKidConcurrent(t *testing.T) {
	keyA := newTestKey(t, "kid-A")
	keyX := newTestKey(t, "kid-X")
	srv := newJWKSServer(t, jwks(keyA))
	v := newOIDCValidator(t, srv.URL)
	t0 := time.Now()
	v.now = func() time.Time { return t0 }

	good := signedFor(t, keyA, t0)
	bad := signedFor(t, keyX, t0)
	const workers, rounds = 16, 25
	var wg sync.WaitGroup
	errCh := make(chan error, workers*rounds)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				if (w+i)%2 == 0 {
					if err := v.Validate(context.Background(), good); err != nil {
						errCh <- fmt.Errorf("valid token rejected: %w", err)
					}
				} else if err := v.Validate(context.Background(), bad); !errors.Is(err, ErrUnauthorized) {
					errCh <- fmt.Errorf("unknown-kid token: expected ErrUnauthorized, got %w", err)
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	// Cold fetch, the unknown-kid refresh, the rebuilt key set's lazy
	// fetch, plus slack for go-oidc's inflight scheduling. Before the fix
	// this grew with the number of unknown-kid tokens.
	if hits := srv.hits.Load(); hits > 6 {
		t.Errorf("expected <=6 JWKS fetches, got %d", hits)
	}
}

func TestNegativeKidCache_BoundedAndExpiring(t *testing.T) {
	c := newNegativeKidCache(4)
	t0 := time.Now()
	for i := 0; i < 10; i++ {
		c.add(fmt.Sprintf("kid-%d", i), t0)
	}
	if n := len(c.entries); n != 4 {
		t.Fatalf("cache holds %d entries; want the 4-entry cap", n)
	}
	if !c.contains("kid-0", t0) {
		t.Fatal("kid-0 should be cached inside its TTL")
	}
	if c.contains("kid-9", t0) {
		t.Fatal("kid-9 arrived with the cache full of live entries and should not have been stored")
	}

	// Once the old entries expire, an add into the full cache sweeps them.
	late := t0.Add(negativeKidTTL)
	c.add("late", late)
	if n := len(c.entries); n != 1 {
		t.Fatalf("cache holds %d entries after expiry sweep; want 1", n)
	}
	if !c.contains("late", late) {
		t.Fatal("late entry should be cached")
	}
	if c.contains("kid-0", late) {
		t.Fatal("kid-0 should have expired at its TTL")
	}
}
