package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"fmt"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
)

// AWSKMSClient is the subset of the AWS KMS API that the signer needs.
// Exposed as an interface so tests can pass a fake client.
type AWSKMSClient interface {
	Sign(ctx context.Context, params *kms.SignInput, optFns ...func(*kms.Options)) (*kms.SignOutput, error)
	GetPublicKey(ctx context.Context, params *kms.GetPublicKeyInput, optFns ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error)
}

// AWSKMSSigner signs JWTs by delegating to an AWS KMS asymmetric key.
// The private key never leaves KMS.
type AWSKMSSigner struct {
	client AWSKMSClient
	keyID  string
	keys   publicKeyCache
}

// NewAWSKMSSigner constructs a signer bound to the given KMS key. The
// keyID may be the key ARN, alias ARN, or alias name -- anything
// accepted by kms:Sign and kms:GetPublicKey. The KMS key must be an
// ECC_NIST_P256 key configured for signing (ES256).
func NewAWSKMSSigner(ctx context.Context, keyID string) (*AWSKMSSigner, error) {
	if keyID == "" {
		return nil, fmt.Errorf("oidc: empty AWS KMS keyID")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("oidc: load aws config: %w", err)
	}
	return NewAWSKMSSignerFromClient(kms.NewFromConfig(cfg), keyID), nil
}

// NewAWSKMSSignerFromClient lets callers (mostly tests) inject a fake
// KMS client. The public key is still fetched lazily via the client.
func NewAWSKMSSignerFromClient(client AWSKMSClient, keyID string) *AWSKMSSigner {
	return &AWSKMSSigner{client: client, keyID: keyID}
}

// Sign calls kms:Sign with the raw SHA-256 digest and the signing
// algorithm ECDSA_SHA_256. AWS KMS returns a DER/ASN.1-encoded ECDSA
// signature, which is converted to the RFC 7518 section 3.4 raw R || S
// form required by ES256 JWS signatures (the Signer.Sign contract).
func (s *AWSKMSSigner) Sign(ctx context.Context, digest []byte) ([]byte, error) {
	out, err := s.client.Sign(ctx, &kms.SignInput{
		KeyId:            &s.keyID,
		Message:          digest,
		MessageType:      types.MessageTypeDigest,
		SigningAlgorithm: types.SigningAlgorithmSpecEcdsaSha256,
	})
	if err != nil {
		return nil, fmt.Errorf("oidc: kms:Sign: %w", err)
	}
	return derToRawECDSASignature(out.Signature)
}

// PublicKey fetches the public half of the KMS key and caches it once a
// fetch succeeds. A failed fetch is returned and retried on the next call.
func (s *AWSKMSSigner) PublicKey(ctx context.Context) (crypto.PublicKey, error) {
	return s.keys.publicKey(ctx, s.fetchPublicKey)
}

// KeyID returns the JWK kid derived from the cached public key.
func (s *AWSKMSSigner) KeyID(ctx context.Context) (string, error) {
	return s.keys.keyID(ctx, s.fetchPublicKey)
}

func (s *AWSKMSSigner) fetchPublicKey(ctx context.Context) (*ecdsa.PublicKey, error) {
	out, err := s.client.GetPublicKey(ctx, &kms.GetPublicKeyInput{KeyId: &s.keyID})
	if err != nil {
		return nil, fmt.Errorf("oidc: kms:GetPublicKey: %w", err)
	}
	pub, err := x509.ParsePKIXPublicKey(out.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("oidc: parse kms public key: %w", err)
	}
	ecPub, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("oidc: kms key is not ECDSA (got %T); key must be ECC_NIST_P256", pub)
	}
	if ecPub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("oidc: kms key curve is %s; key must be ECC_NIST_P256 (ES256)", ecPub.Curve.Params().Name)
	}
	return ecPub, nil
}
