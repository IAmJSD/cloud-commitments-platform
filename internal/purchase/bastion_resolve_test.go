package purchase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/credentials"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	stypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #263: purchases for a bastion-mode account must pass the bastion
// lookup and bastion STS factory to the resolver, so the target role is
// assumed with the bastion's own credentials, never the host's.

const (
	purchBastionID    = "bastion-acct"
	purchBastionKeyID = "BASTIONKEYID"
	purchTargetRole   = "arn:aws:iam::999999999999:role/Target"
)

func purchBastionDeps() (*MockConfigStore, *MockCredentialStore) {
	store := new(MockConfigStore)
	store.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
		if id == purchBastionID {
			return &config.CloudAccount{ID: purchBastionID, Provider: "aws", Enabled: true, AWSAuthMode: "access_keys"}, nil
		}
		return nil, nil
	}
	credStore := &MockCredentialStore{LoadRawFn: func(_ context.Context, accountID, credType string) ([]byte, error) {
		if accountID == purchBastionID && credType == credentials.CredTypeAWSAccessKeys {
			return []byte(`{"access_key_id":"` + purchBastionKeyID + `","secret_access_key":"bastion-test-value"}`), nil
		}
		return nil, nil
	}}
	return store, credStore
}

// refuseHostAssumeRole makes any AssumeRole on the host STS client (a bug for
// a bastion target) return an error instead of a mock panic, so the
// assertions report it.
func refuseHostAssumeRole(m *MockAssumeRoleSTS) {
	m.On("AssumeRole", mock.Anything, mock.Anything).Return(nil, errors.New("host STS client used for a bastion target")).Maybe()
}

func purchBastionTarget() config.CloudAccount {
	return config.CloudAccount{
		ID: "target-acct", Name: "target", Provider: "aws", Enabled: true,
		AWSAuthMode: "bastion", AWSBastionID: purchBastionID, AWSRoleARN: purchTargetRole,
	}
}

func TestResolveAWSProvider_BastionUsesBastionCredentials(t *testing.T) {
	ctx := context.Background()
	store, credStore := purchBastionDeps()
	hostSTS := new(MockAssumeRoleSTS)
	refuseHostAssumeRole(hostSTS)
	bastionSTS := new(MockAssumeRoleSTS)
	bastionSTS.On("AssumeRole", mock.Anything, mock.MatchedBy(func(in *sts.AssumeRoleInput) bool {
		return aws.ToString(in.RoleArn) == purchTargetRole
	})).Return(&sts.AssumeRoleOutput{Credentials: &stypes.Credentials{
		AccessKeyId: aws.String("TARGETSESSIONKEY"), SecretAccessKey: aws.String("s"), SessionToken: aws.String("t"),
		Expiration: aws.Time(time.Now().Add(time.Hour)),
	}}, nil).Once()
	var factoryKey string

	m := NewManager(ManagerConfig{
		ConfigStore:     store,
		CredentialStore: credStore,
		AssumeRoleSTS:   hostSTS,
		BastionSTSFactory: func(p aws.CredentialsProvider) credentials.STSClient {
			creds, err := p.Retrieve(ctx)
			require.NoError(t, err)
			factoryKey = creds.AccessKeyID
			return bastionSTS
		},
	})

	cfg, err := m.resolveAWSProvider(ctx, purchBastionTarget())
	require.NoError(t, err)
	assert.Equal(t, purchBastionKeyID, factoryKey, "bastion STS client must be built from the bastion's own keys")
	store.AssertCalled(t, "GetCloudAccount", mock.Anything, purchBastionID)

	creds, err := cfg.AWSCredentialsProvider.Retrieve(ctx)
	require.NoError(t, err)
	assert.Equal(t, "TARGETSESSIONKEY", creds.AccessKeyID)
	bastionSTS.AssertExpectations(t)
	hostSTS.AssertNotCalled(t, "AssumeRole", mock.Anything, mock.Anything)
}

func TestResolveAWSProvider_BastionWithoutFactoryFailsClosed(t *testing.T) {
	store, credStore := purchBastionDeps()
	hostSTS := new(MockAssumeRoleSTS)
	refuseHostAssumeRole(hostSTS)
	m := NewManager(ManagerConfig{ConfigStore: store, CredentialStore: credStore, AssumeRoleSTS: hostSTS})

	cfg, err := m.resolveAWSProvider(context.Background(), purchBastionTarget())
	require.ErrorContains(t, err, "bastion account lookup and STS client factory are required")
	assert.Nil(t, cfg)
	hostSTS.AssertNotCalled(t, "AssumeRole", mock.Anything, mock.Anything)
}
