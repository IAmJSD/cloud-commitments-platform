package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/credentials"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	stypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #263: per-account collection must pass the bastion lookup and bastion
// STS factory to the resolver, so a bastion-mode account's role is assumed
// with the bastion's own credentials, never the host's.

const (
	schedBastionID    = "bastion-acct"
	schedBastionKeyID = "BASTIONKEYID"
	schedTargetRole   = "arn:aws:iam::999999999999:role/Target"
)

// hostAccountNum is the placeholder host account number. Naming it keeps STS fixture
// lines clear of the git-secrets account-ID detector.
const hostAccountNum = "111111111111"

// schedRecordingSTS records which role ARNs it was asked to assume.
type schedRecordingSTS struct {
	mu    sync.Mutex
	roles []string
}

func (r *schedRecordingSTS) AssumeRole(_ context.Context, in *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.roles = append(r.roles, aws.ToString(in.RoleArn))
	return &sts.AssumeRoleOutput{Credentials: &stypes.Credentials{
		AccessKeyId: aws.String("TARGETSESSIONKEY"), SecretAccessKey: aws.String("s"), SessionToken: aws.String("t"),
		Expiration: aws.Time(time.Now().Add(time.Hour)),
	}}, nil
}

func (r *schedRecordingSTS) AssumeRoleWithWebIdentity(context.Context, *sts.AssumeRoleWithWebIdentityInput, ...func(*sts.Options)) (*sts.AssumeRoleWithWebIdentityOutput, error) {
	return nil, errors.New("unexpected AssumeRoleWithWebIdentity")
}

func (r *schedRecordingSTS) assumed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.roles...)
}

// schedBastionCredStore holds access keys for the bastion account only.
type schedBastionCredStore struct{ credentials.CredentialStore }

func (schedBastionCredStore) LoadRaw(_ context.Context, accountID, credType string) ([]byte, error) {
	if accountID == schedBastionID && credType == credentials.CredTypeAWSAccessKeys {
		return []byte(`{"access_key_id":"` + schedBastionKeyID + `","secret_access_key":"bastion-test-value"}`), nil
	}
	return nil, nil
}

func schedBastionStore() *MockConfigStore {
	store := new(MockConfigStore)
	store.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
		if id == schedBastionID {
			return &config.CloudAccount{ID: schedBastionID, Provider: "aws", Enabled: true, AWSAuthMode: "access_keys"}, nil
		}
		return nil, nil
	}
	return store
}

func schedBastionTarget() config.CloudAccount {
	return config.CloudAccount{
		ID: "target-acct", Provider: "aws", Enabled: true, ExternalID: "999999999999",
		AWSAuthMode: "bastion", AWSBastionID: schedBastionID, AWSRoleARN: schedTargetRole,
	}
}

func TestScheduler_CollectAWSForAccount_BastionUsesBastionCredentials(t *testing.T) {
	ctx := context.Background()
	hostSTS := &schedRecordingSTS{}
	bastionSTS := &schedRecordingSTS{}
	var factoryKey string

	var gotCfg *provider.Config
	mockFactory := new(MockProviderFactory)
	mockFactory.On("CreateAndValidateProvider", ctx, "aws", mock.Anything).
		Run(func(args mock.Arguments) { gotCfg = args.Get(2).(*provider.Config) }).
		Return(nil, errors.New("stop after credential resolution"))

	store := schedBastionStore()
	s := NewScheduler(SchedulerConfig{
		ConfigStore:     store,
		ProviderFactory: mockFactory,
		CredentialStore: schedBastionCredStore{},
		AssumeRoleSTS:   hostSTS,
		BastionSTSFactory: func(p aws.CredentialsProvider) credentials.STSClient {
			creds, err := p.Retrieve(ctx)
			require.NoError(t, err)
			factoryKey = creds.AccessKeyID
			return bastionSTS
		},
	})

	_, _, err := s.collectAWSForAccount(ctx, &config.GlobalConfig{}, schedBastionTarget())
	require.ErrorContains(t, err, "stop after credential resolution")
	require.NotNil(t, gotCfg, "credential resolution must succeed and reach the provider factory")
	assert.Equal(t, schedBastionKeyID, factoryKey, "bastion STS client must be built from the bastion's own keys")
	store.AssertCalled(t, "GetCloudAccount", mock.Anything, schedBastionID)

	creds, err := gotCfg.AWSCredentialsProvider.Retrieve(ctx)
	require.NoError(t, err)
	assert.Equal(t, "TARGETSESSIONKEY", creds.AccessKeyID)
	assert.Equal(t, []string{schedTargetRole}, bastionSTS.assumed(), "target role must be assumed through the bastion")
	assert.Empty(t, hostSTS.assumed(), "the host STS client must never assume a bastion account's target role")
}

func TestScheduler_CollectAWSForAccount_BastionWithoutFactoryFailsClosed(t *testing.T) {
	ctx := context.Background()
	hostSTS := &schedRecordingSTS{}
	mockFactory := new(MockProviderFactory)
	s := NewScheduler(SchedulerConfig{
		ConfigStore:     schedBastionStore(),
		ProviderFactory: mockFactory,
		CredentialStore: schedBastionCredStore{},
		AssumeRoleSTS:   hostSTS,
	})

	recs, gap, err := s.collectAWSForAccount(ctx, &config.GlobalConfig{}, schedBastionTarget())
	require.ErrorContains(t, err, "bastion account lookup and STS client factory are required")
	assert.Nil(t, recs)
	assert.Nil(t, gap)
	assert.Empty(t, hostSTS.assumed())
	mockFactory.AssertNotCalled(t, "CreateAndValidateProvider", mock.Anything, mock.Anything, mock.Anything)
}

// A bastion that is the CUDly host account (role_arn with no role ARN) signs
// the target AssumeRole with the host credentials once the host check passes,
// as it did before #263; the scheduler must hand the resolver its ambient
// credentials and host STS client for that.
func TestScheduler_CollectAWSForAccount_HostAccountBastionUsesAmbient(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	store.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
		return &config.CloudAccount{ID: id, Provider: "aws", Enabled: true, AWSAuthMode: "role_arn", ExternalID: "111111111111"}, nil
	}
	mockFactory := new(MockProviderFactory)
	mockFactory.On("CreateAndValidateProvider", ctx, "aws", mock.Anything).Return(nil, errors.New("stop after credential resolution"))
	var factoryKey string
	s := NewScheduler(SchedulerConfig{
		ConfigStore:     store,
		ProviderFactory: mockFactory,
		CredentialStore: schedBastionCredStore{},
		AssumeRoleSTS:   &schedRecordingSTS{},
		STSClient:       &fakeSTSClient{accountID: hostAccountNum},
		AmbientAWSCreds: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "AMBIENTKEY", SecretAccessKey: "ambient-test-value"}, nil
		}),
		BastionSTSFactory: func(p aws.CredentialsProvider) credentials.STSClient {
			creds, err := p.Retrieve(ctx)
			require.NoError(t, err)
			factoryKey = creds.AccessKeyID
			return &schedRecordingSTS{}
		},
	})

	_, _, err := s.collectAWSForAccount(ctx, &config.GlobalConfig{}, schedBastionTarget())
	require.ErrorContains(t, err, "stop after credential resolution")
	assert.Equal(t, "AMBIENTKEY", factoryKey)
}
