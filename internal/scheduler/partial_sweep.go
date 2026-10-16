package scheduler

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	awsrecommendations "github.com/LeanerCloud/cloud-commitments-go/providers/aws/recommendations"
	azureprovider "github.com/LeanerCloud/cloud-commitments-go/providers/azure"
	gcpprovider "github.com/LeanerCloud/cloud-commitments-go/providers/gcp"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"google.golang.org/api/googleapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// sweepGap describes what an incomplete recommendation sweep missed, in the
// sanitized form that is persisted to recommendations_state and served to the
// dashboard (issue #87). A nil *sweepGap means the sweep was complete.
//
// It carries counts and reason categories only. The raw provider errors and
// the failed subscription/project IDs stay in the scheduler's WARN log: the
// state row is global and readable by every view:recommendations user, while
// accounts are access-scoped, so naming them there would leak which
// subscriptions exist to users not authorized for them.
type sweepGap struct {
	incompleteAccounts int
	attemptedScopes    int
	failedScopes       int
	failedDetails      int
	reasons            map[string]int
}

// gapFromAWSIncomplete summarizes an AWS IncompleteRecommendationsError.
// AWS does not report how many scopes it attempted, so attemptedScopes is 0.
func gapFromAWSIncomplete(e *awsrecommendations.IncompleteRecommendationsError) *sweepGap {
	g := &sweepGap{
		failedScopes:  e.FailedScopes,
		failedDetails: e.FailedDetails,
		reasons:       map[string]int{},
	}
	for _, cause := range e.Causes {
		g.reasons[classifySweepCause(cause)]++
	}
	return g
}

// gapFromPartialSubscriptions summarizes a multi-subscription fan-out that
// lost some subscriptions (Azure subscriptions, or GCP projects routed
// through the same error type).
func gapFromPartialSubscriptions(p *azureprovider.PartialSubscriptionFailureError) *sweepGap {
	g := &sweepGap{
		attemptedScopes: p.Attempted,
		failedScopes:    max(p.Attempted-p.Succeeded, len(p.Failed)),
		reasons:         map[string]int{},
	}
	for _, f := range p.Failed {
		g.reasons[classifySweepCause(f.Err)]++
	}
	return g
}

// forAccount marks the gap as one account's incomplete sweep. Scope-level
// gaps (for example the AWS primary fetch and its fallback for the same
// account) are merged first and counted as an account once, here.
func (g *sweepGap) forAccount() *sweepGap {
	if g == nil {
		return nil
	}
	out := mergeSweepGaps(nil, g)
	out.incompleteAccounts = 1
	return out
}

// mergeSweepGaps returns the sum of two gaps without mutating either. nil is
// the identity: merging two complete sweeps stays complete.
func mergeSweepGaps(a, b *sweepGap) *sweepGap {
	if a == nil && b == nil {
		return nil
	}
	out := &sweepGap{reasons: map[string]int{}}
	for _, g := range []*sweepGap{a, b} {
		if g == nil {
			continue
		}
		out.incompleteAccounts += g.incompleteAccounts
		out.attemptedScopes += g.attemptedScopes
		out.failedScopes += g.failedScopes
		out.failedDetails += g.failedDetails
		for reason, n := range g.reasons {
			out.reasons[reason] += n
		}
	}
	return out
}

// toProviderSweep converts the gap into its persisted, API-facing shape.
func (g *sweepGap) toProviderSweep(providerName string) config.PartialProviderSweep {
	reasons := make(map[string]int, len(g.reasons))
	for reason, n := range g.reasons {
		reasons[reason] = n
	}
	return config.PartialProviderSweep{
		Provider:           providerName,
		IncompleteAccounts: g.incompleteAccounts,
		AttemptedScopes:    g.attemptedScopes,
		FailedScopes:       g.failedScopes,
		FailedDetails:      g.failedDetails,
		Reasons:            reasons,
	}
}

// classifySweepCause maps one failed scope's error to a closed reason
// category so the API never echoes provider error text (which can carry
// ARNs, subscription IDs or request IDs).
func classifySweepCause(err error) string {
	switch {
	case err == nil:
		return config.PartialReasonOther
	case errors.Is(err, context.Canceled):
		return config.PartialReasonCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return config.PartialReasonTimeout
	case gcpprovider.IsPermissionError(err):
		return config.PartialReasonPermissionDenied
	}
	// AWS throttling and access errors arrive as HTTP 400 with a telling
	// error code, so the code is checked before the status.
	if reason := reasonForErrorCode(err); reason != "" {
		return reason
	}
	if code, ok := httpStatusOf(err); ok {
		return reasonForHTTPStatus(code)
	}
	if s, ok := status.FromError(err); ok {
		return reasonForGRPCCode(s.Code())
	}
	return config.PartialReasonOther
}

// reasonForErrorCode classifies AWS (smithy APIError) and Azure error codes.
// It returns "" when the code says nothing classifiable.
func reasonForErrorCode(err error) string {
	var code string
	var apiErr interface{ ErrorCode() string }
	var azErr *azcore.ResponseError
	switch {
	case errors.As(err, &apiErr):
		code = apiErr.ErrorCode()
	case errors.As(err, &azErr):
		code = azErr.ErrorCode
	default:
		return ""
	}
	code = strings.ToLower(code)
	switch {
	case strings.Contains(code, "throttl"), strings.Contains(code, "toomanyrequests"),
		strings.Contains(code, "requestlimitexceeded"):
		return config.PartialReasonThrottled
	case strings.Contains(code, "accessdenied"), strings.Contains(code, "unauthorized"),
		strings.Contains(code, "authorizationfailed"), strings.Contains(code, "forbidden"):
		return config.PartialReasonPermissionDenied
	default:
		return ""
	}
}

// httpStatusOf extracts an HTTP status from the Azure, GCP REST or AWS
// error types in err's chain.
func httpStatusOf(err error) (int, bool) {
	var azErr *azcore.ResponseError
	if errors.As(err, &azErr) {
		return azErr.StatusCode, true
	}
	var gErr *googleapi.Error
	if errors.As(err, &gErr) {
		return gErr.Code, true
	}
	var awsErr interface{ HTTPStatusCode() int }
	if errors.As(err, &awsErr) {
		return awsErr.HTTPStatusCode(), true
	}
	return 0, false
}

func reasonForHTTPStatus(code int) string {
	switch {
	case code == http.StatusUnauthorized, code == http.StatusForbidden:
		return config.PartialReasonPermissionDenied
	case code == http.StatusNotFound:
		return config.PartialReasonNotFound
	case code == http.StatusRequestTimeout, code == http.StatusGatewayTimeout:
		return config.PartialReasonTimeout
	case code == http.StatusTooManyRequests:
		return config.PartialReasonThrottled
	case code >= http.StatusInternalServerError:
		return config.PartialReasonProviderUnavailable
	default:
		return config.PartialReasonOther
	}
}

func reasonForGRPCCode(code codes.Code) string {
	switch code {
	case codes.PermissionDenied, codes.Unauthenticated:
		return config.PartialReasonPermissionDenied
	case codes.ResourceExhausted:
		return config.PartialReasonThrottled
	case codes.DeadlineExceeded:
		return config.PartialReasonTimeout
	case codes.Canceled:
		return config.PartialReasonCanceled
	case codes.NotFound:
		return config.PartialReasonNotFound
	case codes.Unavailable, codes.Internal:
		return config.PartialReasonProviderUnavailable
	default:
		return config.PartialReasonOther
	}
}
