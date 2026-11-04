package scheduler

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
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
	// skippedRecommendations counts recommendations the provider dropped as
	// invalid (for example non-USD) while returning the rest. A gap made only
	// of these does not hold back stale-row eviction; see holdsEviction.
	skippedRecommendations int
	// skippedOnly is true when every sweep merged into this gap lost nothing
	// but invalid recommendations, so the account's sweep itself was complete.
	skippedOnly bool
	reasons     map[string]int
	// pricingUnavailable is set when a skip meant the price could not be
	// fetched, so the previous cycle's rows are held (see hasPricingOutageSkip).
	pricingUnavailable bool
	// heldAccountIDs are the platform account UUIDs whose rows are held for
	// that reason; the collector that knows the account fills them in.
	heldAccountIDs []string
}

// holdsEviction reports whether the sweep must stay out of the succeeded-
// account roster. A nil gap (complete sweep) and a skipped-only gap do not:
// a skipped recommendation is invalid, so its previous-cycle row is exactly
// what stale-row eviction should remove rather than keep offering for
// purchase. Every other gap lost whole scopes, whose rows must survive.
func (g *sweepGap) holdsEviction() bool {
	return g != nil && !g.skippedOnly
}

// partialResult is implemented by provider errors that accompany usable
// results (GCP's skipped recommendations). Matching the method, not the
// provider's internal type, keeps this provider-agnostic.
type partialResult interface {
	Partial() bool
	SkippedCount() int
	SkipReasons() []string
}

// tolerateSkippedRecommendations keeps the recommendations returned alongside
// a Partial() error and records how many were skipped. Any other error is
// returned unchanged.
func tolerateSkippedRecommendations(providerName string, err error) (*sweepGap, error) {
	var p partialResult
	if err == nil || !errors.As(err, &p) || !p.Partial() {
		return nil, err
	}
	logging.Warnf("%s recommendations partial, keeping the rest: %v", providerName, err)
	g := &sweepGap{reasons: map[string]int{}}
	g.recordSkips(p.SkippedCount(), p.SkipReasons())
	g.skippedOnly = !g.pricingUnavailable
	return g, nil
}

// trimSubscriptionPrefixes drops the "<subscription ID>: " that the Azure
// multi-subscription client puts before each skip it aggregates
// (recommendations_multi_subscription.go:399 at 61111428), so the reasons match
// the same prefixes as a single-subscription sweep. Only a leading segment
// followed by an Advisor skip is removed, so unprefixed reasons pass through.
func trimSubscriptionPrefixes(reasons []string) []string {
	out := make([]string, len(reasons))
	for i, reason := range reasons {
		out[i] = reason
		if sub, rest, ok := strings.Cut(reason, ": "); ok && sub != "" && !strings.ContainsAny(sub, " :") && strings.HasPrefix(rest, "advisor: ") {
			out[i] = rest
		}
	}
	return out
}

// recordSkips adds dropped recommendations to the gap. A skip that means "the
// price could not be fetched" says nothing about the row being invalid, so its
// previous-cycle row must survive: it marks the gap pricingUnavailable.
func (g *sweepGap) recordSkips(count int, reasons []string) {
	g.skippedRecommendations += count
	if outage := countPricingOutageSkips(trimSubscriptionPrefixes(reasons)); outage > 0 {
		g.pricingUnavailable = true
		g.reasons[config.PartialReasonPricingUnavailable] += outage
	}
}

// withHeld records accountID as held when the gap is a pricing outage. An
// empty accountID (an unresolved ambient identity) cannot be named.
func (g *sweepGap) withHeld(accountID string) *sweepGap {
	if g == nil || !g.pricingUnavailable || accountID == "" {
		return g
	}
	out := mergeSweepGaps(nil, g)
	out.heldAccountIDs = append(out.heldAccountIDs, accountID)
	return out
}

// Skip-reason prefixes cloud-commitments-go records when Azure Advisor rows
// cannot be priced because the Retail Prices catalog is unreachable
// (providers/azure/recommendations_advisor_pricing.go, priceAdvisorReservations,
// go#663). The library exports no constant for them: RecordSkip stores
// "<service>: <err>" strings. The first failed row is recorded with the
// collection-failure wrapper "azure advisor: "; every later row of the call
// gets "price lookup unavailable: ".
var pricingOutageSkipPrefixes = []string{
	"advisor: price lookup unavailable: ",
	"advisor: azure advisor: ",
}

// advisorTruncatedSkipPrefix starts the skip recorded for an Advisor walk that
// hit the page cap or a page error (collectAdvisorOrSkip,
// providers/azure/recommendations.go:471 at 61111428). It shares the
// "advisor: azure advisor: " prefix but is not a pricing outage: a walk over
// the page cap is always truncated, so holding eviction would keep that
// account's stale rows forever.
const advisorTruncatedSkipPrefix = "advisor: azure advisor: recommendation list is incomplete"

// hasPricingOutageSkip reports whether any skip reason is a pricing outage.
func hasPricingOutageSkip(reasons []string) bool {
	return countPricingOutageSkips(reasons) > 0
}

func countPricingOutageSkips(reasons []string) int {
	n := 0
	for _, reason := range reasons {
		if strings.HasPrefix(reason, advisorTruncatedSkipPrefix) {
			continue
		}
		for _, prefix := range pricingOutageSkipPrefixes {
			if strings.HasPrefix(reason, prefix) {
				n++
				break
			}
		}
	}
	return n
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
	// A mixed sweep also lost invalid recommendations. The count is for the
	// user warning only and does not affect eviction (skippedOnly stays false),
	// but a pricing outage among them holds the account like a skips-only one.
	if sk := p.Skips(); sk != nil {
		g.recordSkips(sk.SkippedCount(), sk.SkipReasons())
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
	if g.holdsEviction() {
		out.incompleteAccounts = 1
	}
	return out
}

// mergeSweepGaps returns the sum of two gaps without mutating either. nil is
// the identity: merging two complete sweeps stays complete.
func mergeSweepGaps(a, b *sweepGap) *sweepGap {
	if a == nil && b == nil {
		return nil
	}
	out := &sweepGap{reasons: map[string]int{}, skippedOnly: true}
	for _, g := range []*sweepGap{a, b} {
		if g == nil {
			continue
		}
		out.skippedOnly = out.skippedOnly && g.skippedOnly
		out.pricingUnavailable = out.pricingUnavailable || g.pricingUnavailable
		out.heldAccountIDs = append(out.heldAccountIDs, g.heldAccountIDs...)
		out.skippedRecommendations += g.skippedRecommendations
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
		Provider:               providerName,
		IncompleteAccounts:     g.incompleteAccounts,
		AttemptedScopes:        g.attemptedScopes,
		FailedScopes:           g.failedScopes,
		FailedDetails:          g.failedDetails,
		SkippedRecommendations: g.skippedRecommendations,
		Reasons:                reasons,
		HeldAccountIDs:         slices.Sorted(slices.Values(g.heldAccountIDs)),
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
