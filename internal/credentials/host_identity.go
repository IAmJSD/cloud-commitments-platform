package credentials

import (
	"context"
	"errors"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// hostIdentityTimeout bounds the STS GetCallerIdentity call. A variable so
// tests can shorten it.
var hostIdentityTimeout = 5 * time.Second

// ErrNotHostAccount means a role_arn account with no role ARN could not be
// confirmed as the AWS account CUDly's own credentials belong to. Its text is
// stored in tenant-visible rows, so it never carries the host account id or
// raw STS errors; those are logged. VerifyHostAccount returns it as such only
// when STS names a different account, the one case the tenant can fix by
// setting aws_role_arn; ErrHostIdentityUnavailable also matches it.
var ErrNotHostAccount = errors.New("credentials: aws_role_arn is empty but the account is not confirmed as the CUDly host account; set aws_role_arn")

// ErrHostIdentityUnavailable means the host identity check could not run: no
// STS client, an STS error or timeout (a canceled caller included), or an
// empty STS answer. The cause is on the platform side, so unlike
// ErrNotHostAccount its tenant-visible text asks for no config change.
// errors.Is also matches it against ErrNotHostAccount, so every caller keeps
// failing closed exactly as before (issue #559).
var ErrHostIdentityUnavailable error = hostIdentityUnavailableError{}

type hostIdentityUnavailableError struct{}

func (hostIdentityUnavailableError) Error() string {
	return "credentials: the CUDly host account could not be verified because of a platform-side error; no change to this account's configuration is needed, retry later"
}

// Is reports the error as ErrNotHostAccount too: the account is still not
// confirmed as the host, so callers that refuse on ErrNotHostAccount refuse.
func (hostIdentityUnavailableError) Is(target error) bool { return target == ErrNotHostAccount }

// CallerIdentityClient is satisfied by *sts.Client.
type CallerIdentityClient interface {
	GetCallerIdentity(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// VerifyHostAccount confirms account.ExternalID is the AWS account that hostSTS,
// an STS client signed with the host's ambient credentials, runs as. An
// account mismatch returns ErrNotHostAccount; a check that could not run
// returns ErrHostIdentityUnavailable. Both are returned unwrapped; the detail
// goes to the log only. The STS call inherits ctx, so a canceled caller stops
// it, and is bounded by hostIdentityTimeout.
func VerifyHostAccount(ctx context.Context, account *config.CloudAccount, hostSTS CallerIdentityClient) error {
	if hostSTS == nil {
		logging.Warnf("host identity check for account %s: no STS client to resolve the host identity", account.ID)
		return ErrHostIdentityUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, hostIdentityTimeout)
	defer cancel()
	out, err := hostSTS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		logging.Warnf("host identity check for account %s: resolve host identity: %v", account.ID, err)
		return ErrHostIdentityUnavailable
	}
	if out == nil || out.Account == nil || *out.Account == "" {
		logging.Warnf("host identity check for account %s: STS returned no host account ID", account.ID)
		return ErrHostIdentityUnavailable
	}
	if account.ExternalID != *out.Account {
		logging.Warnf("host identity check for account %s: external_id %q does not match host account %s", account.ID, account.ExternalID, *out.Account)
		return ErrNotHostAccount
	}
	return nil
}
