package oidc

import (
	"context"
	"crypto/ecdsa"
	"testing"
	"time"
)

// Issue #104: the KMS-backed signers memoized their public key for the
// process lifetime. The AWS signer signs through a key alias and the Azure
// signer through the key's current version, so after a rotation Sign used the
// new key while the JWKS kept publishing the old one and Mint kept stamping
// the old kid until the instance recycled. The cache now expires after
// publicKeyCacheTTL.

// testClock is a manually advanced clock for publicKeyCache.now.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time { return c.t }

func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// rotatingSignerBackend builds a KMS-backed signer whose backing key a test
// can swap, standing in for an operator rotating the alias / current version.
type rotatingSignerBackend struct {
	build func(key *ecdsa.PrivateKey, gate *keyLookupGate, clock *testClock) (signer Signer, rotate func(*ecdsa.PrivateKey))
	name  string
}

func rotatingSignerBackends() []rotatingSignerBackend {
	return []rotatingSignerBackend{
		{name: "aws", build: func(key *ecdsa.PrivateKey, gate *keyLookupGate, clock *testClock) (Signer, func(*ecdsa.PrivateKey)) {
			fake := &fakeKMSClient{key: key}
			s := NewAWSKMSSignerFromClient(gatedAWSKMS{fake, gate}, "alias/test-key")
			s.keys.now = clock.now
			return s, func(k *ecdsa.PrivateKey) { fake.key = k }
		}},
		{name: "azure", build: func(key *ecdsa.PrivateKey, gate *keyLookupGate, clock *testClock) (Signer, func(*ecdsa.PrivateKey)) {
			fake := &fakeAzureKeyVaultClient{key: &key.PublicKey}
			s := NewAzureKeyVaultSignerFromClient(gatedAzureKeyVault{fake, gate}, "test-key", "")
			s.keys.now = clock.now
			return s, func(k *ecdsa.PrivateKey) { fake.key = &k.PublicKey }
		}},
	}
}

func jwksKid(t *testing.T, signer Signer) string {
	t.Helper()
	jwks, err := BuildJWKS(context.Background(), signer)
	if err != nil {
		t.Fatalf("BuildJWKS: %v", err)
	}
	if len(jwks.Keys) != 1 {
		t.Fatalf("JWKS has %d keys, want 1", len(jwks.Keys))
	}
	return jwks.Keys[0].Kid
}

func TestKMSSignerPublishesRotatedKeyAfterTTL(t *testing.T) {
	for _, backend := range rotatingSignerBackends() {
		t.Run(backend.name, func(t *testing.T) {
			oldKey, oldKid := newP256TestKey(t)
			newKey, newKid := newP256TestKey(t)
			gate := &keyLookupGate{}
			clock := &testClock{t: time.Unix(1_700_000_000, 0)}
			signer, rotate := backend.build(oldKey, gate, clock)

			if got := jwksKid(t, signer); got != oldKid {
				t.Fatalf("initial JWKS kid = %q, want %q", got, oldKid)
			}
			rotate(newKey)

			clock.advance(publicKeyCacheTTL - time.Second)
			if got := jwksKid(t, signer); got != oldKid {
				t.Errorf("JWKS kid before the TTL = %q, want the cached %q", got, oldKid)
			}
			if got := gate.calls.Load(); got != 1 {
				t.Errorf("key lookups before the TTL = %d, want 1 (cached)", got)
			}

			clock.advance(2 * time.Second)
			if got := jwksKid(t, signer); got != newKid {
				t.Errorf("JWKS kid after the TTL = %q, want the rotated %q", got, newKid)
			}
			if kid, err := signer.KeyID(context.Background()); err != nil || kid != newKid {
				t.Errorf("KeyID after the TTL = %q, %v; want %q", kid, err, newKid)
			}
			if got := gate.calls.Load(); got != 2 {
				t.Errorf("key lookups = %d, want 2 (one refresh after the TTL)", got)
			}
		})
	}
}

func TestKMSSignerServesStaleKeyWhenRefreshFails(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	refreshes := []struct {
		ctx      context.Context
		name     string
		failures int32
	}{
		{name: "transient error", ctx: context.Background(), failures: 1},
		{name: "canceled caller", ctx: canceled, failures: 0},
	}
	for _, backend := range rotatingSignerBackends() {
		for _, refresh := range refreshes {
			t.Run(backend.name+"/"+refresh.name, func(t *testing.T) {
				ctx := context.Background()
				oldKey, oldKid := newP256TestKey(t)
				newKey, newKid := newP256TestKey(t)
				gate := &keyLookupGate{}
				clock := &testClock{t: time.Unix(1_700_000_000, 0)}
				signer, rotate := backend.build(oldKey, gate, clock)

				if kid, err := signer.KeyID(ctx); err != nil || kid != oldKid {
					t.Fatalf("initial KeyID = %q, %v; want %q", kid, err, oldKid)
				}
				rotate(newKey)
				clock.advance(publicKeyCacheTTL)

				gate.failures.Store(refresh.failures)
				if kid, err := signer.KeyID(refresh.ctx); err != nil || kid != oldKid {
					t.Fatalf("KeyID on a failed refresh = %q, %v; want the stale %q and no error", kid, err, oldKid)
				}
				if got := gate.calls.Load(); got != 2 {
					t.Fatalf("key lookups = %d, want 2 (the failed refresh was attempted)", got)
				}

				// Within the retry interval the stale key is served without a fetch.
				clock.advance(publicKeyRefreshRetryInterval - time.Second)
				if kid, err := signer.KeyID(ctx); err != nil || kid != oldKid {
					t.Errorf("KeyID within the retry interval = %q, %v; want the stale %q", kid, err, oldKid)
				}
				if got := gate.calls.Load(); got != 2 {
					t.Errorf("key lookups within the retry interval = %d, want 2", got)
				}

				clock.advance(time.Second)
				if kid, err := signer.KeyID(ctx); err != nil || kid != newKid {
					t.Errorf("KeyID after the retry interval = %q, %v; want the rotated %q", kid, err, newKid)
				}
				if got := gate.calls.Load(); got != 3 {
					t.Errorf("key lookups = %d, want 3 (one successful retry)", got)
				}
			})
		}
	}
}
