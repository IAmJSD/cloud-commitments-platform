package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/credentials"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// awsAccountProbeTimeout bounds the credential retrieval plus the
// sts:GetCallerIdentity probe run by the account test endpoint.
const awsAccountProbeTimeout = 15 * time.Second

// awsResolverCredResult tests an AWS account through the same credential
// resolver (and options) the purchase and marketplace paths use, then proves
// the resolved credentials work with an sts:GetCallerIdentity call. Any config
// the resolver rejects (unsupported auth mode, missing bastion id, missing
// web-identity token file, non-host role_arn account without a role ARN,
// incomplete stored access keys, ...) is reported as OK=false instead of the
// shape-only "configured" answer the endpoint used to give (issue #722).
func (h *Handler) awsResolverCredResult(ctx context.Context, acct *config.CloudAccount) AccountTestResult {
	base, err := h.getBaseAWSConfig(ctx)
	if err != nil {
		return AccountTestResult{OK: false, Message: fmt.Sprintf("could not load the CUDly host AWS config: %v", err)}
	}
	if base.Region == "" {
		// STS needs a region to build an endpoint; the global partition
		// endpoint is reachable from us-east-1.
		base.Region = "us-east-1"
	}
	hostSTS := sts.NewFromConfig(base)
	provider, err := credentials.ResolveAWSCredentialProviderWithOpts(ctx, acct, h.credStore, hostSTS,
		credentials.AWSResolveOptions{AmbientProvider: base.Credentials, AmbientSTS: hostSTS})
	if err != nil {
		if errors.Is(err, credentials.ErrNotHostAccount) {
			return AccountTestResult{OK: false, Message: credentials.ErrNotHostAccount.Error()}
		}
		return AccountTestResult{OK: false, Message: fmt.Sprintf("credential resolution failed: %v", err)}
	}
	return probeAWSCallerIdentity(ctx, base, provider)
}

// probeAWSCallerIdentity retrieves credentials from provider (which performs
// any AssumeRole / AssumeRoleWithWebIdentity exchange) and signs an
// sts:GetCallerIdentity call with them.
func probeAWSCallerIdentity(ctx context.Context, base aws.Config, provider aws.CredentialsProvider) AccountTestResult {
	probeCtx, cancel := context.WithTimeout(ctx, awsAccountProbeTimeout)
	defer cancel()

	cache := aws.NewCredentialsCache(provider)
	if _, err := cache.Retrieve(probeCtx); err != nil {
		return AccountTestResult{OK: false, Message: fmt.Sprintf("credential retrieval failed: %v", err)}
	}
	cfg := base.Copy()
	cfg.Credentials = cache
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(probeCtx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return AccountTestResult{OK: false, Message: fmt.Sprintf("sts:GetCallerIdentity failed: %v", err)}
	}
	if out == nil || out.Arn == nil || *out.Arn == "" {
		return AccountTestResult{OK: false, Message: "sts:GetCallerIdentity returned no caller identity"}
	}
	return AccountTestResult{OK: true, Message: fmt.Sprintf("credentials validated (sts:GetCallerIdentity succeeded as %s)", *out.Arn)}
}
