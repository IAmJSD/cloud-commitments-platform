package credentials

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	stypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testBastionID    = "bastion-acct"
	testBastionKeyID = "BASTIONKEYID"
	testTargetRole   = "arn:aws:iam::999999999999:role/Target"
)

// recordingSTS records AssumeRole calls and hands out fixed session keys.
type recordingSTS struct {
	mu              sync.Mutex
	assumeRoleCalls int
	roleARNs        []string
}

func (r *recordingSTS) AssumeRole(_ context.Context, in *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.assumeRoleCalls++
	r.roleARNs = append(r.roleARNs, aws.ToString(in.RoleArn))
	return &sts.AssumeRoleOutput{Credentials: &stypes.Credentials{
		AccessKeyId:     aws.String("TARGETSESSIONKEY"),
		SecretAccessKey: aws.String("target-session-secret"),
		SessionToken:    aws.String("target-session-token"),
		Expiration:      aws.Time(time.Now().Add(time.Hour)),
	}}, nil
}

func (r *recordingSTS) AssumeRoleWithWebIdentity(_ context.Context, _ *sts.AssumeRoleWithWebIdentityInput, _ ...func(*sts.Options)) (*sts.AssumeRoleWithWebIdentityOutput, error) {
	return nil, assert.AnError
}

// accessKeyBastionFixture returns a store holding access keys for an enabled
// access_keys bastion and options that look it up and build a mock STS client.
func accessKeyBastionFixture() (*mockCredentialStore, AWSResolveOptions) {
	store := newMockStore()
	store.data[testBastionID+"/"+CredTypeAWSAccessKeys] = []byte(`{"access_key_id":"` + testBastionKeyID + `","secret_access_key":"bastion-test-value"}`)
	bastion := &config.CloudAccount{ID: testBastionID, Enabled: true, AWSAuthMode: "access_keys"}
	return store, AWSResolveOptions{
		AccountLookup: func(_ context.Context, id string) (*config.CloudAccount, error) {
			if id == testBastionID {
				return bastion, nil
			}
			return nil, nil
		},
		STSClientFactory: func(_ aws.CredentialsProvider) STSClient { return &mockSTSClient{} },
	}
}

func bastionTarget() *config.CloudAccount {
	return &config.CloudAccount{ID: "target-acct", AWSAuthMode: "bastion", AWSBastionID: testBastionID, AWSRoleARN: testTargetRole}
}

// The target role must be assumed with the STS client the factory built from
// the bastion's credentials, never with the caller's (host) STS client.
func TestResolveBastionProvider_TargetAssumedWithBastionSTS(t *testing.T) {
	store, opts := accessKeyBastionFixture()
	bastionSTS := &recordingSTS{}
	var bastionCreds aws.Credentials
	opts.STSClientFactory = func(p aws.CredentialsProvider) STSClient {
		var err error
		bastionCreds, err = p.Retrieve(context.Background())
		require.NoError(t, err)
		return bastionSTS
	}
	hostSTS := &recordingSTS{}

	provider, err := ResolveAWSCredentialProviderWithOpts(context.Background(), bastionTarget(), store, hostSTS, opts)
	require.NoError(t, err)
	assert.Equal(t, testBastionKeyID, bastionCreds.AccessKeyID, "factory must be given the bastion's own credentials")

	creds, err := provider.Retrieve(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "TARGETSESSIONKEY", creds.AccessKeyID)
	assert.Equal(t, []string{testTargetRole}, bastionSTS.roleARNs, "target role must be assumed through the bastion STS client")
	assert.Zero(t, hostSTS.assumeRoleCalls, "the caller's STS client must not assume the target role")
}

// A bastion that is the CUDly host account (role_arn with no role ARN) keeps
// working: its credentials are the ambient ones, once the host check passes.
// A bastion of that shape that is not the host is refused.
func TestResolveBastionProvider_HostAccountBastion(t *testing.T) {
	hostBastion := &config.CloudAccount{ID: testBastionID, Enabled: true, AWSAuthMode: "role_arn", ExternalID: "111111111111"}
	newOpts := func(hostAccount string, gotProvider *aws.CredentialsProvider) AWSResolveOptions {
		return AWSResolveOptions{
			AccountLookup: func(_ context.Context, _ string) (*config.CloudAccount, error) { return hostBastion, nil },
			STSClientFactory: func(p aws.CredentialsProvider) STSClient {
				*gotProvider = p
				return &recordingSTS{}
			},
			AmbientProvider: testAmbientCreds,
			AmbientSTS:      &callerIdentitySTS{account: hostAccount},
		}
	}

	t.Run("host account", func(t *testing.T) {
		var got aws.CredentialsProvider
		_, err := ResolveAWSCredentialProviderWithOpts(context.Background(), bastionTarget(), newMockStore(), &recordingSTS{}, newOpts("111111111111", &got))
		require.NoError(t, err)
		require.NotNil(t, got)
		creds, err := got.Retrieve(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "AMBIENTKEY", creds.AccessKeyID)
	})
	t.Run("not the host account", func(t *testing.T) {
		var got aws.CredentialsProvider
		_, err := ResolveAWSCredentialProviderWithOpts(context.Background(), bastionTarget(), newMockStore(), &recordingSTS{}, newOpts("222222222222", &got))
		require.ErrorIs(t, err, ErrNotHostAccount)
		assert.Nil(t, got, "no STS client may be built for an unverified bastion")
	})
}

type stubAccountGetter struct{ ids []string }

func (s *stubAccountGetter) GetCloudAccount(_ context.Context, id string) (*config.CloudAccount, error) {
	s.ids = append(s.ids, id)
	return &config.CloudAccount{ID: id}, nil
}

func TestAccountLookupFromStore(t *testing.T) {
	assert.Nil(t, AccountLookupFromStore(nil), "a nil store must give a nil lookup so bastion resolution fails closed")

	store := &stubAccountGetter{}
	lookup := AccountLookupFromStore(store)
	require.NotNil(t, lookup)
	acct, err := lookup(context.Background(), testBastionID)
	require.NoError(t, err)
	assert.Equal(t, testBastionID, acct.ID)
	assert.Equal(t, []string{testBastionID}, store.ids)
}

// The factory's client must sign with the provider it is given (the bastion's
// credentials), not with base's own credentials, and keep base's region.
func TestNewSTSClientFactory_SignsWithGivenProvider(t *testing.T) {
	base := aws.Config{Region: "eu-west-1", Credentials: testAmbientCreds}
	bastion := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: testBastionKeyID, SecretAccessKey: "bastion-test-value"}, nil
	})

	for name, p := range map[string]aws.CredentialsProvider{
		"plain provider":  bastion,
		"cached provider": aws.NewCredentialsCache(bastion),
	} {
		t.Run(name, func(t *testing.T) {
			client, ok := NewSTSClientFactory(base)(p).(*sts.Client)
			require.True(t, ok)
			o := client.Options()
			assert.Equal(t, "eu-west-1", o.Region)
			cache, ok := o.Credentials.(*aws.CredentialsCache)
			require.True(t, ok, "credentials must be cached so a role-based bastion is not re-assumed per call")
			assert.True(t, cache.IsCredentialsProvider(bastion), "cache must wrap the given provider directly")
			creds, err := o.Credentials.Retrieve(context.Background())
			require.NoError(t, err)
			assert.Equal(t, testBastionKeyID, creds.AccessKeyID)
		})
	}
	baseCreds, err := base.Credentials.Retrieve(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "AMBIENTKEY", baseCreds.AccessKeyID, "base config must not be modified")
}
