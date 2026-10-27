package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/credentials"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #263: the commitment-options probe must pass the bastion lookup and a
// bastion STS factory to the resolver. The STS endpoint is faked at the HTTP
// layer, so no request leaves the process.

const (
	probeBastionID    = "bastion-acct"
	probeBastionKeyID = "BASTIONKEYID"
	probeHostKeyID    = "HOSTKEYID"
	probeTargetRole   = "arn:aws:iam::999999999999:role/Target"
)

// fakeSTSEndpoint answers AssumeRole and records the access key that signed it.
type fakeSTSEndpoint struct {
	mu      sync.Mutex
	signers []string
}

func (f *fakeSTSEndpoint) Do(r *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(b), "Action=AssumeRole") {
		return nil, errors.New("unexpected STS call: " + string(b))
	}
	auth := r.Header.Get("Authorization")
	f.mu.Lock()
	switch {
	case strings.Contains(auth, "Credential="+probeBastionKeyID+"/"):
		f.signers = append(f.signers, probeBastionKeyID)
	case strings.Contains(auth, "Credential="+probeHostKeyID+"/"):
		f.signers = append(f.signers, probeHostKeyID)
	default:
		f.signers = append(f.signers, "unknown")
	}
	f.mu.Unlock()
	body := `<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult><Credentials>` +
		`<AccessKeyId>TARGETSESSIONKEY</AccessKeyId><SecretAccessKey>s</SecretAccessKey><SessionToken>t</SessionToken>` +
		`<Expiration>2099-01-01T00:00:00Z</Expiration></Credentials><AssumedRoleUser><AssumedRoleId>A:s</AssumedRoleId>` +
		`<Arn>arn:aws:sts::999999999999:assumed-role/Target/s</Arn></AssumedRoleUser></AssumeRoleResult>` +
		`<ResponseMetadata><RequestId>r</RequestId></ResponseMetadata></AssumeRoleResponse>`
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{"Content-Type": []string{"text/xml"}}}, nil
}

type bastionAccountStore struct{ ids []string }

func (s *bastionAccountStore) GetCloudAccount(_ context.Context, id string) (*config.CloudAccount, error) {
	s.ids = append(s.ids, id)
	if id == probeBastionID {
		return &config.CloudAccount{ID: probeBastionID, Provider: "aws", Enabled: true, AWSAuthMode: "access_keys"}, nil
	}
	return nil, nil
}

// bastionKeyStore serves access keys for the bastion only; other methods are
// unused and would panic on the nil embedded interface.
type bastionKeyStore struct{ credentials.CredentialStore }

func (bastionKeyStore) LoadRaw(_ context.Context, accountID, _ string) ([]byte, error) {
	if accountID == probeBastionID {
		return []byte(`{"access_key_id":"` + probeBastionKeyID + `","secret_access_key":"bastion-test-value"}`), nil
	}
	return nil, nil
}

func TestCommitmentProbeConfigBuilder_BastionAssumesTargetAsBastion(t *testing.T) {
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	endpoint := &fakeSTSEndpoint{}
	hostCfg := aws.Config{
		Region: "us-east-1",
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: probeHostKeyID, SecretAccessKey: "host-test-value"}, nil
		}),
		HTTPClient:       endpoint,
		RetryMaxAttempts: 1,
	}
	accounts := &bastionAccountStore{}
	build := newCommitmentProbeConfigBuilder(bastionKeyStore{}, accounts, hostCfg, hostIdentitySTS{account: "111111111111"})

	target := &config.CloudAccount{ID: "target-acct", Provider: "aws", AWSAuthMode: "bastion", AWSBastionID: probeBastionID, AWSRoleARN: probeTargetRole}
	cfg, err := build(context.Background(), target)
	require.NoError(t, err, "a bastion account must resolve once the lookup and factory are wired")
	assert.Equal(t, []string{probeBastionID}, accounts.ids, "bastion account must be loaded from the config store")

	creds, err := cfg.Credentials.Retrieve(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "TARGETSESSIONKEY", creds.AccessKeyID)
	assert.Equal(t, []string{probeBastionKeyID}, endpoint.signers, "target role must be assumed with the bastion's keys, never the host's")
}

func TestCommitmentProbeConfigBuilder_BastionWithoutAccountStoreFailsClosed(t *testing.T) {
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	build := newCommitmentProbeConfigBuilder(bastionKeyStore{}, nil, aws.Config{Credentials: aws.AnonymousCredentials{}}, hostIdentitySTS{account: "111111111111"})
	target := &config.CloudAccount{ID: "target-acct", Provider: "aws", AWSAuthMode: "bastion", AWSBastionID: probeBastionID, AWSRoleARN: probeTargetRole}
	_, err := build(context.Background(), target)
	require.ErrorContains(t, err, "bastion account lookup and STS client factory are required")
}
