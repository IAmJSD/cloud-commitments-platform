package server

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/credentials"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type hostIdentitySTS struct{ account string }

func (s hostIdentitySTS) AssumeRole(context.Context, *sts.AssumeRoleInput, ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	return nil, assert.AnError
}

func (s hostIdentitySTS) AssumeRoleWithWebIdentity(context.Context, *sts.AssumeRoleWithWebIdentityInput, ...func(*sts.Options)) (*sts.AssumeRoleWithWebIdentityOutput, error) {
	return nil, assert.AnError
}

func (s hostIdentitySTS) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	return &sts.GetCallerIdentityOutput{Account: aws.String(s.account)}, nil
}

// Guards the AmbientSTS wiring: without it a role_arn account with no role ARN
// would be rejected even when it is the host, or (if the check were bypassed)
// accepted when it is not.
func TestCommitmentProbeConfigBuilder_HostIdentityCheck(t *testing.T) {
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	build := newCommitmentProbeConfigBuilder(nil, nil, aws.Config{Credentials: aws.AnonymousCredentials{}}, hostIdentitySTS{account: "111111111111"})

	nonHost := &config.CloudAccount{ID: "tenant", ExternalID: "222222222222", AWSAuthMode: "role_arn"}
	_, err := build(context.Background(), nonHost)
	require.ErrorIs(t, err, credentials.ErrNotHostAccount)

	host := &config.CloudAccount{ID: "host", ExternalID: "111111111111", AWSAuthMode: "role_arn"}
	cfg, err := build(context.Background(), host)
	require.NoError(t, err)
	assert.Equal(t, "us-east-1", cfg.Region)
}
