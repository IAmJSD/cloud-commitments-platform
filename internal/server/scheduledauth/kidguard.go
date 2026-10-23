package scheduledauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
)

// Negative kid cache (issue #75).
//
// go-oidc's RemoteKeySet refetches the JWKS every time a token fails to
// verify against its cached keys, with no cooldown. Each bad token is
// therefore proxied 1:1 to the provider's JWKS endpoint. guardedKeySet
// wraps the RemoteKeySet and remembers, for negativeKidTTL, any kid that a
// JWKS fetch issued AFTER the token arrived could not verify. Further
// tokens with that kid are rejected without calling go-oidc, so a repeated
// kid costs at most one JWKS fetch per negativeKidTTL.
//
// Safety properties:
//   - Fail closed: a cache hit only ever rejects; it never accepts.
//   - A kid is cached only when a JWKS fetch that started after the token
//     arrived completed successfully and still could not verify it. A failed
//     fetch, or one that was already in flight when the token arrived (the
//     go-oidc stale-inflight case), never caches anything.
//   - A cached kid never blocks a token the latest fetched JWKS can verify,
//     so forged tokens that reuse a published kid cannot lock out genuine
//     tokens carrying that kid, and a rotated key is accepted as soon as any
//     refresh picks it up.
//   - Entries expire after negativeKidTTL (the same window as the verifier
//     rebuild limit), and the cache holds at most maxNegativeKids entries,
//     keyed by a SHA-256 of the kid so attacker-sized kids cost 32 bytes.
//
// Revisit if go-oidc adds its own refresh cooldown: bump it and drop this.

// negativeKidTTL is how long a kid that a fresh JWKS fetch did not contain
// is rejected without a refetch. It matches minRebuildInterval, so the
// negative cache never delays rotation recovery beyond the existing bound.
const negativeKidTTL = minRebuildInterval

// maxNegativeKids caps the negative cache. When it is full of live entries
// a new kid is not stored; that kid then behaves as it did before the cache
// existed (one fetch per token), so overflow costs no correctness.
const maxNegativeKids = 1024

// errKidRecentlyAbsent is returned for a token whose kid a recent JWKS
// fetch did not contain. go-oidc wraps it as "failed to verify signature".
var errKidRecentlyAbsent = errors.New("token key ID was absent from a recent JWKS fetch; not refetching")

// jwksSnapshot is the key set returned by one successful JWKS fetch.
type jwksSnapshot struct {
	keys []jose.JSONWebKey
	seq  uint64 // jwksObserver.started value when this fetch began
}

// jwksObserver is the RoundTripper for the validator's JWKS client. It
// numbers every fetch as it starts and records the key set from each
// successful response, so guardedKeySet can tell whether a fetch began
// after a given token arrived and what that fetch returned. The client is
// used only for the JWKS URL (go-oidc fetches and Warmup).
type jwksObserver struct {
	base    http.RoundTripper
	started atomic.Uint64
	latest  atomic.Pointer[jwksSnapshot]
}

// RoundTrip forwards to the hardened base transport, then buffers and
// parses a 200 response body before handing it back unchanged.
func (o *jwksObserver) RoundTrip(req *http.Request) (*http.Response, error) {
	seq := o.started.Add(1)
	resp, err := o.base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return resp, err
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("reading JWKS response: %w", err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	o.record(seq, body)
	return resp, nil
}

// record stores the parsed key set unless a fetch that started later has
// already been recorded. Unparseable bodies are ignored: go-oidc rejects
// them too, so they never become its cached keys.
func (o *jwksObserver) record(seq uint64, body []byte) {
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(body, &set); err != nil {
		return
	}
	snap := &jwksSnapshot{keys: set.Keys, seq: seq}
	for {
		cur := o.latest.Load()
		if cur != nil && cur.seq >= seq {
			return
		}
		if o.latest.CompareAndSwap(cur, snap) {
			return
		}
	}
}

// negativeKidCache is a size-bounded set of kids with per-entry expiry.
type negativeKidCache struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]time.Time // kid hash -> expiry
	max     int
}

func newNegativeKidCache(maxEntries int) *negativeKidCache {
	return &negativeKidCache{entries: make(map[[sha256.Size]byte]time.Time), max: maxEntries}
}

// contains reports whether kid has a live entry at now, dropping it if it
// has expired.
func (c *negativeKidCache) contains(kid string, now time.Time) bool {
	h := sha256.Sum256([]byte(kid))
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.entries[h]
	if !ok {
		return false
	}
	if now.Before(exp) {
		return true
	}
	delete(c.entries, h)
	return false
}

// add records kid until now+negativeKidTTL. When the cache is full it first
// sweeps expired entries; if it is still full the kid is not stored.
func (c *negativeKidCache) add(kid string, now time.Time) {
	h := sha256.Sum256([]byte(kid))
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[h]; !ok && len(c.entries) >= c.max {
		for k, exp := range c.entries {
			if !now.Before(exp) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= c.max {
			return
		}
	}
	c.entries[h] = now.Add(negativeKidTTL)
}

// kidGuard is the validator-wide state shared by every guardedKeySet, so
// it survives verifier rebuilds.
type kidGuard struct {
	observer *jwksObserver
	negative *negativeKidCache
	now      func() time.Time
	algs     []jose.SignatureAlgorithm
}

// newKidGuard installs a jwksObserver in front of client's transport.
func newKidGuard(client *http.Client, now func() time.Time, algs []jose.SignatureAlgorithm) *kidGuard {
	obs := &jwksObserver{base: client.Transport}
	client.Transport = obs
	return &kidGuard{
		observer: obs,
		negative: newNegativeKidCache(maxNegativeKids),
		now:      now,
		algs:     algs,
	}
}

// guardedKeySet is the oidc.KeySet handed to the IDTokenVerifier.
type guardedKeySet struct {
	inner oidc.KeySet
	g     *kidGuard
}

// VerifySignature rejects tokens whose kid is negatively cached and that
// the latest JWKS cannot verify; everything else goes to go-oidc. When
// go-oidc fails, the kid is cached if a fresh fetch confirmed it unknown.
func (k *guardedKeySet) VerifySignature(ctx context.Context, rawJWT string) ([]byte, error) {
	arrival := k.g.observer.started.Load()
	jws, err := jose.ParseSigned(rawJWT, k.g.algs)
	if err != nil {
		return nil, fmt.Errorf("malformed jwt: %w", err)
	}
	kid := jwsKeyID(jws)
	if k.g.negative.contains(kid, k.g.now()) && !k.g.latestVerifies(jws, kid) {
		return nil, errKidRecentlyAbsent
	}
	payload, err := k.inner.VerifySignature(ctx, rawJWT)
	if err != nil {
		k.g.rememberIfConfirmed(jws, kid, arrival)
	}
	return payload, err
}

// latestVerifies reports whether the most recently fetched JWKS holds a
// key that verifies jws.
func (g *kidGuard) latestVerifies(jws *jose.JSONWebSignature, kid string) bool {
	snap := g.observer.latest.Load()
	return snap != nil && verifiesWith(jws, kid, snap.keys)
}

// rememberIfConfirmed caches kid when a JWKS fetch that started after the
// token arrived (seq > arrival) completed and still cannot verify it.
func (g *kidGuard) rememberIfConfirmed(jws *jose.JSONWebSignature, kid string, arrival uint64) {
	snap := g.observer.latest.Load()
	if snap == nil || snap.seq <= arrival || verifiesWith(jws, kid, snap.keys) {
		return
	}
	g.negative.add(kid, g.now())
}

// verifiesWith mirrors go-oidc's key selection: keys whose kid matches, or
// every key when the token has no kid.
func verifiesWith(jws *jose.JSONWebSignature, kid string, keys []jose.JSONWebKey) bool {
	for i := range keys {
		if kid != "" && keys[i].KeyID != kid {
			continue
		}
		if _, err := jws.Verify(&keys[i]); err == nil {
			return true
		}
	}
	return false
}

// jwsKeyID returns the kid of the first signature, as go-oidc does.
func jwsKeyID(jws *jose.JSONWebSignature) string {
	if len(jws.Signatures) == 0 {
		return ""
	}
	return jws.Signatures[0].Header.KeyID
}
