package oidc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/golang-jwt/jwt/v5"
)

// assertRawES256JWS verifies that jws is a compact JWS whose signature
// segment is the RFC 7518 section 3.4 raw R || S form (exactly 64 bytes
// for P-256) and that it verifies against pub. It checks the signature
// three independent ways so the test fails on the pre-fix DER-emitting
// code: (1) the decoded signature is exactly 64 bytes; (2) the R || S
// split verifies via ecdsa.Verify; (3) a real JOSE/JWT ES256 parser
// (golang-jwt) accepts the token, proving real OIDC consumers like
// Azure AD would too.
func assertRawES256JWS(t *testing.T, jws string, pub *ecdsa.PublicKey) {
	t.Helper()

	parts := strings.Split(jws, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWS parts, got %d", len(parts))
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	// (1) Raw R || S is exactly 64 bytes for P-256. DER signatures are
	// ~70-72 bytes and start with 0x30, so this rejects the pre-fix output.
	if len(sig) != 64 {
		t.Fatalf("JWS signature is %d bytes, want 64-byte raw R||S (RFC 7518 ES256); first byte 0x%02x", len(sig), sig[0])
	}

	// (2) Split R || S and verify with the public key over the signing input.
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, digest[:], r, s) {
		t.Errorf("raw R||S ECDSA signature verify failed")
	}

	// (3) A real JOSE/JWT ES256 parser must accept the token.
	if _, err := jwt.Parse(jws, func(*jwt.Token) (any, error) {
		return pub, nil
	}, jwt.WithValidMethods([]string{"ES256"}), jwt.WithoutClaimsValidation()); err != nil {
		t.Errorf("golang-jwt ES256 parse/verify failed: %v", err)
	}
}

func TestLocalSignerMintAndVerify(t *testing.T) {
	ctx := context.Background()
	signer, err := NewLocalSigner()
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}

	claims := map[string]any{
		"iss": "https://cudly.example.com",
		"sub": "cudly-controller",
		"aud": "api://AzureADTokenExchange",
		"exp": 9999999999,
	}

	jws, err := Mint(ctx, signer, claims)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	parts := strings.Split(jws, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWS parts, got %d", len(parts))
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var header map[string]any
	if umErr := json.Unmarshal(headerBytes, &header); umErr != nil {
		t.Fatalf("unmarshal header: %v", umErr)
	}
	// Positively assert ES256 -- this is the invariant the fix-422 change guards.
	if header["alg"] != "ES256" {
		t.Errorf("alg=%v, want ES256 (PKCS1v15/RS256 is no longer permitted)", header["alg"])
	}
	if header["typ"] != "JWT" {
		t.Errorf("typ=%v, want JWT", header["typ"])
	}
	expectedKid, _ := signer.KeyID(ctx)
	if header["kid"] != expectedKid {
		t.Errorf("kid mismatch: header=%v signer=%v", header["kid"], expectedKid)
	}

	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var decoded map[string]any
	if umErr := json.Unmarshal(claimsBytes, &decoded); umErr != nil {
		t.Fatalf("unmarshal claims: %v", umErr)
	}
	if decoded["iss"] != claims["iss"] {
		t.Errorf("iss mismatch: %v vs %v", decoded["iss"], claims["iss"])
	}

	// Verify the JWS signature end-to-end. The signature MUST be the
	// RFC 7518 section 3.4 raw R || S form (64 bytes for P-256), not DER.
	rawPub, err := signer.PublicKey(ctx)
	if err != nil {
		t.Fatalf("public key: %v", err)
	}
	ecPub, ok := rawPub.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("public key is not *ecdsa.PublicKey, got %T", rawPub)
	}
	assertRawES256JWS(t, jws, ecPub)
}

func TestBuildJWKS(t *testing.T) {
	ctx := context.Background()
	signer, err := NewLocalSigner()
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	jwks, err := BuildJWKS(ctx, signer)
	if err != nil {
		t.Fatalf("build jwks: %v", err)
	}
	if len(jwks.Keys) != 1 {
		t.Fatalf("want 1 key, got %d", len(jwks.Keys))
	}
	k := jwks.Keys[0]
	// Positively assert EC/ES256 -- guards against regression back to RSA/RS256.
	if k.Kty != "EC" || k.Use != "sig" || k.Alg != "ES256" {
		t.Errorf("jwk metadata wrong: %+v", k)
	}
	if k.Crv != "P-256" {
		t.Errorf("jwk crv wrong: got %q, want P-256", k.Crv)
	}
	if k.Kid == "" || k.X == "" || k.Y == "" {
		t.Errorf("jwk missing kid/x/y: %+v", k)
	}
	if _, err := base64.RawURLEncoding.DecodeString(k.X); err != nil {
		t.Errorf("x not base64url: %v", err)
	}
	if _, err := base64.RawURLEncoding.DecodeString(k.Y); err != nil {
		t.Errorf("y not base64url: %v", err)
	}
	// RSA fields are structurally absent from the EC JWK type.
}

// TestPublicJWKRejectsWrongCurve guards against a misconfigured
// non-P-256 KMS/Key Vault key (e.g. P-384) silently producing a
// corrupt JWK: pre-fix, PublicJWK hardcoded Crv/Alg to "P-256"/ES256
// regardless of ecPub.Curve, so a P-384 key would be published as a
// mislabeled P-256 JWK with 48-byte (not 32-byte) coordinates, and the
// misconfiguration would only surface later as an opaque KMS signing
// failure. Post-fix, PublicJWK must reject it immediately.
func TestPublicJWKRejectsWrongCurve(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("gen p384 key: %v", err)
	}
	if _, err := PublicJWK(&key.PublicKey, "some-kid"); err == nil {
		t.Fatal("PublicJWK accepted a P-384 key; want an error rejecting the wrong curve")
	}
}

func TestBuildDiscovery(t *testing.T) {
	d := BuildDiscovery("https://cudly.example.com")
	if d.Issuer != "https://cudly.example.com" {
		t.Errorf("issuer=%s", d.Issuer)
	}
	if d.JWKSURI != "https://cudly.example.com/.well-known/jwks.json" {
		t.Errorf("jwks_uri=%s", d.JWKSURI)
	}
	// Positively assert ES256 in the discovery document.
	if len(d.IDTokenSigningAlgValuesSupported) != 1 || d.IDTokenSigningAlgValuesSupported[0] != "ES256" {
		t.Errorf("alg values wrong: %v, want [ES256]", d.IDTokenSigningAlgValuesSupported)
	}
}

func TestComputeKeyIDStableAcrossCalls(t *testing.T) {
	signer, err := NewLocalSigner()
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	a, err := signer.KeyID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := signer.KeyID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("kid changed between calls: %s vs %s", a, b)
	}
}

func TestComputeKeyIDChangesOnKeyRotation(t *testing.T) {
	s1, err := NewLocalSigner()
	if err != nil {
		t.Fatalf("new signer 1: %v", err)
	}
	s2, err := NewLocalSigner()
	if err != nil {
		t.Fatalf("new signer 2: %v", err)
	}
	k1, _ := s1.KeyID(context.Background())
	k2, _ := s2.KeyID(context.Background())
	if k1 == k2 {
		t.Errorf("different keys produced the same kid: %s", k1)
	}
}

// Audit finding A03-014 (issue #263): each KMS-backed signer resolved its
// public key inside sync.Once and stored the error with it, so one failed
// first lookup (a throttle, a network blip, or a caller whose ctx was already
// canceled) poisoned the process-wide signer until the instance recycled.
// Only a successful resolution may be cached.

var errTransientKeyLookup = errors.New("transient key lookup failure")

// keyLookupGate makes a fake backend's public-key fetch fail while failures
// is positive (and whenever the caller's ctx is done), and counts every
// fetch so a test can tell a retried lookup from a memoized one.
type keyLookupGate struct {
	failures atomic.Int32
	calls    atomic.Int32
}

func (g *keyLookupGate) enter(ctx context.Context) error {
	g.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return err
	}
	if g.failures.Add(-1) >= 0 {
		return errTransientKeyLookup
	}
	return nil
}

type gatedAWSKMS struct {
	*fakeKMSClient
	gate *keyLookupGate
}

func (c gatedAWSKMS) GetPublicKey(ctx context.Context, in *kms.GetPublicKeyInput, opts ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error) {
	if err := c.gate.enter(ctx); err != nil {
		return nil, err
	}
	return c.fakeKMSClient.GetPublicKey(ctx, in, opts...)
}

type gatedAzureKeyVault struct {
	*fakeAzureKeyVaultClient
	gate *keyLookupGate
}

func (c gatedAzureKeyVault) GetKey(ctx context.Context, name, version string, opts *azkeys.GetKeyOptions) (azkeys.GetKeyResponse, error) {
	if err := c.gate.enter(ctx); err != nil {
		return azkeys.GetKeyResponse{}, err
	}
	return c.fakeAzureKeyVaultClient.GetKey(ctx, name, version, opts)
}

type gatedGCPKMS struct {
	*fakeGCPKMSClient
	gate *keyLookupGate
}

func (c gatedGCPKMS) GetPublicKey(ctx context.Context, req *kmspb.GetPublicKeyRequest, opts ...interface{}) (*kmspb.PublicKey, error) {
	if err := c.gate.enter(ctx); err != nil {
		return nil, err
	}
	return c.fakeGCPKMSClient.GetPublicKey(ctx, req, opts...)
}

type gatedSignerBackend struct {
	build func(key *ecdsa.PrivateKey, gate *keyLookupGate) Signer
	name  string
}

func gatedSignerBackends() []gatedSignerBackend {
	return []gatedSignerBackend{
		{name: "aws", build: func(key *ecdsa.PrivateKey, gate *keyLookupGate) Signer {
			return NewAWSKMSSignerFromClient(gatedAWSKMS{&fakeKMSClient{key: key}, gate}, "alias/test-key")
		}},
		{name: "azure", build: func(key *ecdsa.PrivateKey, gate *keyLookupGate) Signer {
			return NewAzureKeyVaultSignerFromClient(gatedAzureKeyVault{&fakeAzureKeyVaultClient{key: &key.PublicKey}, gate}, "test-key", "")
		}},
		{name: "gcp", build: func(key *ecdsa.PrivateKey, gate *keyLookupGate) Signer {
			return NewGCPKMSSignerFromClient(gatedGCPKMS{&fakeGCPKMSClient{key: key}, gate},
				"projects/p/locations/global/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1")
		}},
	}
}

func newP256TestKey(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	kid, err := ComputeKeyID(&key.PublicKey)
	if err != nil {
		t.Fatalf("kid: %v", err)
	}
	return key, kid
}

func TestKMSSignerRetriesFailedKeyLookup(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	firstCalls := []struct {
		ctx      context.Context
		wantErr  error
		name     string
		failures int32
	}{
		{name: "transient error", ctx: context.Background(), failures: 1, wantErr: errTransientKeyLookup},
		{name: "canceled first caller", ctx: canceled, failures: 0, wantErr: context.Canceled},
	}
	for _, backend := range gatedSignerBackends() {
		for _, first := range firstCalls {
			t.Run(backend.name+"/"+first.name, func(t *testing.T) {
				ctx := context.Background()
				key, wantKid := newP256TestKey(t)
				gate := &keyLookupGate{}
				gate.failures.Store(first.failures)
				signer := backend.build(key, gate)

				if _, err := signer.KeyID(first.ctx); !errors.Is(err, first.wantErr) {
					t.Fatalf("first KeyID err = %v, want %v", err, first.wantErr)
				}

				pub, err := signer.PublicKey(ctx)
				if err != nil {
					t.Fatalf("PublicKey after a failed first lookup: %v (the failure was cached)", err)
				}
				if ecPub, ok := pub.(*ecdsa.PublicKey); !ok || !ecPub.Equal(&key.PublicKey) {
					t.Fatalf("PublicKey = %v, want the backend key", pub)
				}
				kid, err := signer.KeyID(ctx)
				if err != nil || kid != wantKid {
					t.Fatalf("KeyID = %q, %v; want %q", kid, err, wantKid)
				}
				// One failed fetch, one successful fetch, then the cache.
				if got := gate.calls.Load(); got != 2 {
					t.Errorf("key lookups = %d, want 2 (retry once, then memoize success)", got)
				}
			})
		}
	}
}

// TestKMSSignerConcurrentFirstResolution races many first callers against a
// backend whose first fetch fails: only the caller that drew the failure may
// see it, the rest must get the key, and the success is fetched exactly once.
func TestKMSSignerConcurrentFirstResolution(t *testing.T) {
	const callers = 16
	for _, backend := range gatedSignerBackends() {
		t.Run(backend.name, func(t *testing.T) {
			ctx := context.Background()
			key, wantKid := newP256TestKey(t)
			gate := &keyLookupGate{}
			gate.failures.Store(1)
			signer := backend.build(key, gate)

			var (
				start  sync.WaitGroup
				done   sync.WaitGroup
				failed atomic.Int32
				wrong  atomic.Int32
			)
			start.Add(1)
			for range callers {
				done.Add(1)
				go func() {
					defer done.Done()
					start.Wait()
					kid, err := signer.KeyID(ctx)
					switch {
					case errors.Is(err, errTransientKeyLookup):
						failed.Add(1)
					case err != nil || kid != wantKid:
						wrong.Add(1)
					}
				}()
			}
			start.Done()
			done.Wait()

			if got := failed.Load(); got != 1 {
				t.Errorf("callers that saw the transient failure = %d, want 1", got)
			}
			if got := wrong.Load(); got != 0 {
				t.Errorf("callers with an unexpected result = %d, want 0", got)
			}
			if got := gate.calls.Load(); got != 2 {
				t.Errorf("key lookups = %d, want 2 (one failure, one memoized success)", got)
			}
		})
	}
}
