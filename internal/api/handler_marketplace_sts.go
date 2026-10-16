package api

import (
	"context"
	"errors"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/smithy-go"
)

// Fixed messages for transient STS AssumeRole failures (issue #561). Like
// marketplaceRoleAssumptionMessage, they never carry the AWS error text, which
// names the host account and role ARNs.
const (
	marketplaceRoleThrottledMessage   = "AWS throttled the role assumption for this account; retry shortly"
	marketplaceRoleUnavailableMessage = "could not reach AWS to assume the role configured for this account; retry shortly"
	marketplaceRoleRetryAfterSeconds  = "5"
)

// The SDK's own classifiers decide what counts as throttling and as a
// transient (retryable) failure, so this stays in step with its retry policy.
var (
	stsThrottleChecks  = retry.IsErrorThrottles(retry.DefaultThrottles)
	stsRetryableChecks = retry.IsErrorRetryables(retry.DefaultRetryables)
)

// roleAssumptionClientError maps a failed STS AssumeRole operation, classified
// on that operation's own error chain. Throttling becomes a 503 with
// Retry-After, other transient failures (STS 5xx or server faults, network
// errors, timeouts) a 502, and everything else (access denied, bad trust
// policy, STS disabled in the region, ...) keeps the fixed 400 telling the
// caller to check the role.
// The full error is logged server-side only.
func roleAssumptionClientError(opMsg string, opErr *smithy.OperationError) error {
	err := opErr.Err
	switch {
	case stsThrottleChecks.IsErrorThrottle(err) == aws.TrueTernary:
		logging.Warnf("marketplace: %s: STS AssumeRole throttled: %v", opMsg, opErr)
		return NewClientErrorWithHeaders(503, marketplaceRoleThrottledMessage, nil,
			map[string]string{"Retry-After": marketplaceRoleRetryAfterSeconds})
	case isTransientSTSError(err):
		logging.Warnf("marketplace: %s: transient STS AssumeRole failure: %v", opMsg, opErr)
		return NewClientError(502, marketplaceRoleUnavailableMessage)
	default:
		logging.Warnf("marketplace: %s: STS AssumeRole failed: %v", opMsg, opErr)
		return NewClientError(400, marketplaceRoleAssumptionMessage)
	}
}

// isTransientSTSError reports whether err is a server-side or connectivity
// failure rather than a problem with the role configuration.
func isTransientSTSError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorFault() == smithy.FaultServer {
		return true
	}
	return stsRetryableChecks.IsErrorRetryable(err) == aws.TrueTernary
}
