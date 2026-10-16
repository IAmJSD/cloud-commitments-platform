package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/credentials"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

// awsAccountProbeTimeout bounds the credential retrieval plus the
// sts:GetCallerIdentity probe run by the account test endpoint. A variable so
// tests can shorten it.
var awsAccountProbeTimeout = 15 * time.Second

// awsResolverCredResult tests an AWS account through the same credential
// resolver (and options) the purchase and marketplace paths use, then proves
// the resolved credentials work with an sts:GetCallerIdentity call whose
// account must match the row's external_id. Any config the resolver rejects
// is reported as OK=false instead of the shape-only "configured" answer the
// endpoint used to give (issue #722).
//
// Messages are fixed and classified: raw resolver, store, SDK and STS error
// text (which can name the CUDly host's role ARN and account, request IDs or
// driver errors) is logged server-side only, never returned to the caller.
func (h *Handler) awsResolverCredResult(ctx context.Context, acct *config.CloudAccount) AccountTestResult {
	base, err := h.getBaseAWSConfig(ctx)
	if err != nil {
		logging.Warnf("account test %s: load host AWS config: %v", acct.ID, err)
		return AccountTestResult{OK: false, Message: "CUDly host AWS configuration unavailable"}
	}
	if base.Region == "" {
		// Intentional: STS only needs some region to build an endpoint, and
		// GetCallerIdentity / AssumeRole answer from any of them, so a missing
		// host region must not fail the test. us-east-1 serves the global
		// STS endpoint for the aws partition.
		base.Region = "us-east-1"
	}
	hostSTS := sts.NewFromConfig(base)
	provider, err := credentials.ResolveAWSCredentialProviderWithOpts(ctx, acct, h.credStore, hostSTS,
		credentials.AWSResolveOptions{AmbientProvider: base.Credentials, AmbientSTS: hostSTS})
	if err != nil {
		logging.Warnf("account test %s: resolve AWS credentials: %v", acct.ID, err)
		return AccountTestResult{OK: false, Message: classifyAWSResolveError(err)}
	}
	return probeAWSCallerIdentity(ctx, base, provider, acct)
}

// classifyAWSResolveError maps a resolver error to a fixed, tenant-safe message.
func classifyAWSResolveError(err error) string {
	if errors.Is(err, credentials.ErrNotHostAccount) {
		return "aws_role_arn is empty but this account is not the CUDly host account; set aws_role_arn"
	}
	var cfgErr *credentials.AWSConfigError
	if errors.As(err, &cfgErr) {
		if cfgErr.Problem == credentials.ProblemNotStored {
			return "credentials not stored"
		}
		return fmt.Sprintf("credential configuration incomplete: %s %s", cfgErr.Field, cfgErr.Problem)
	}
	return "credential resolution failed (details in server logs)"
}

// probeAWSCallerIdentity retrieves credentials from provider (which performs
// any AssumeRole / AssumeRoleWithWebIdentity exchange), signs an
// sts:GetCallerIdentity call with them and checks the returned account is the
// row's external_id.
func probeAWSCallerIdentity(ctx context.Context, base aws.Config, provider aws.CredentialsProvider, acct *config.CloudAccount) AccountTestResult {
	probeCtx, cancel := context.WithTimeout(ctx, awsAccountProbeTimeout)
	defer cancel()

	cache := aws.NewCredentialsCache(provider)
	if _, err := cache.Retrieve(probeCtx); err != nil {
		logging.Warnf("account test %s: retrieve AWS credentials: %v", acct.ID, err)
		return AccountTestResult{OK: false, Message: classifyAWSRetrieveError(probeCtx, err, acct)}
	}
	cfg := base.Copy()
	cfg.Credentials = cache
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(probeCtx, &sts.GetCallerIdentityInput{})
	if err != nil {
		logging.Warnf("account test %s: sts:GetCallerIdentity: %v", acct.ID, err)
		return AccountTestResult{OK: false, Message: classifyAWSSTSError(probeCtx, err, "credentials rejected by STS")}
	}
	if out == nil || out.Account == nil || *out.Account == "" {
		logging.Warnf("account test %s: sts:GetCallerIdentity returned no account", acct.ID)
		return AccountTestResult{OK: false, Message: "sts:GetCallerIdentity returned no caller identity"}
	}
	if *out.Account != acct.ExternalID {
		logging.Warnf("account test %s: credentials resolve to AWS account %s, external_id is %q", acct.ID, *out.Account, acct.ExternalID)
		return AccountTestResult{OK: false, Message: fmt.Sprintf("credentials belong to a different AWS account than external_id %s", acct.ExternalID)}
	}
	return AccountTestResult{OK: true, Message: fmt.Sprintf("credentials validated (sts:GetCallerIdentity succeeded for AWS account %s)", acct.ExternalID)}
}

// classifyAWSRetrieveError maps a credential retrieval failure (the
// AssumeRole / AssumeRoleWithWebIdentity step) to a fixed message that names
// at most the role ARN already stored on the row.
func classifyAWSRetrieveError(ctx context.Context, err error, acct *config.CloudAccount) string {
	// stscreds does not wrap the token-file read error (it formats it with
	// %v), so match the SDK's fixed prefix for that failure as well.
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) || strings.Contains(err.Error(), "failed to retrieve jwt from provide source") {
		return "web identity token file unreadable"
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && acct.AWSRoleARN != "" {
		switch apiErr.ErrorCode() {
		case "AccessDenied", "AccessDeniedException":
			return fmt.Sprintf("assume role denied for %s", acct.AWSRoleARN)
		}
	}
	return classifyAWSSTSError(ctx, err, "credential retrieval failed")
}

// classifyAWSSTSError maps an STS call failure to a fixed message. An API
// error contributes only its error code (an AWS-defined identifier), never
// its message text.
func classifyAWSSTSError(ctx context.Context, err error, prefix string) string {
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return "STS probe timed out"
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return fmt.Sprintf("%s (%s)", prefix, apiErr.ErrorCode())
	}
	return "STS unreachable"
}
