package api

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #263: every API path that resolves AWS credentials for a cloud account
// passes the bastion lookup and a bastion STS factory, so a bastion-mode
// account's target role is assumed with the bastion's own keys, never the
// host's. STS is faked at the HTTP layer (probeSTS); nothing leaves the process.
// The account test endpoint is covered in handler_accounts_awsprobe_test.go.

const apiBastionID = "bastion-1"

func apiBastionTarget() *config.CloudAccount {
	return &config.CloudAccount{
		ID: probeAccountID, Provider: "aws", Enabled: true, ExternalID: probeTenantAcct,
		AWSAuthMode: "bastion", AWSBastionID: apiBastionID, AWSRoleARN: probeTenantRole,
		AWSIsOrgRoot: true,
	}
}

// apiBastionHandler returns a handler whose config store knows the bastion (an
// access_keys account whose stored keys are probeMemberKeyID) and whose host
// AWS config signs with probeHostKeyID against fake.
func apiBastionHandler(fake *probeSTS, withStore bool) (*Handler, aws.Config) {
	base := aws.Config{
		Region:           "us-east-1",
		Credentials:      awscreds.NewStaticCredentialsProvider(probeHostKeyID, "host-secret", ""),
		HTTPClient:       fake,
		RetryMaxAttempts: 1,
	}
	h := &Handler{credStore: &probeCredStore{payload: validAccessKeyPayload}}
	if withStore {
		store := new(MockConfigStore)
		store.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
			if id == apiBastionID {
				return &config.CloudAccount{ID: apiBastionID, Provider: "aws", Enabled: true, AWSAuthMode: "access_keys"}, nil
			}
			return nil, nil
		}
		h.config = store
	}
	h.awsCfgOnce.Do(func() { h.awsCfg = base })
	return h, base
}

var apiBastionCallers = map[string]func(ctx context.Context, h *Handler, base aws.Config) (aws.Config, error){
	"marketplace": func(ctx context.Context, h *Handler, base aws.Config) (aws.Config, error) {
		return h.marketplaceAccountAWSConfig(ctx, apiBastionTarget(), base)
	},
	"org discovery root": func(ctx context.Context, h *Handler, _ aws.Config) (aws.Config, error) {
		return h.buildOrgRootAWSConfig(ctx, apiBastionTarget())
	},
}

func TestAPIAWSCallers_BastionAssumesTargetAsBastion(t *testing.T) {
	for name, call := range apiBastionCallers {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			fake := &probeSTS{}
			h, base := apiBastionHandler(fake, true)

			cfg, err := call(ctx, h, base)
			require.NoError(t, err)
			creds, err := cfg.Credentials.Retrieve(ctx)
			require.NoError(t, err)
			assert.Equal(t, probeAssumedKey, creds.AccessKeyID)
			assert.Equal(t, []string{probeMemberKeyID}, fake.assumeRoleSigners(),
				"target role must be assumed with the bastion's keys, never the host's")
		})
	}
}

func TestAPIAWSCallers_BastionWithoutConfigStoreFailsClosed(t *testing.T) {
	for name, call := range apiBastionCallers {
		t.Run(name, func(t *testing.T) {
			fake := &probeSTS{}
			h, base := apiBastionHandler(fake, false)

			_, err := call(context.Background(), h, base)
			require.ErrorContains(t, err, "bastion account lookup and STS client factory are required")
			assert.Zero(t, fake.assumeRoles.Load(), "no AssumeRole may be sent with the host's credentials")
		})
	}
}
