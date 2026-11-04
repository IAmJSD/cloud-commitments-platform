package purchase

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"google.golang.org/api/googleapi"
)

// Closed set of purchase failure categories shown to users in place of the
// provider's own error text, which carries ARNs, account IDs and request IDs.
const (
	ErrCategoryAccessDenied = "access_denied"
	ErrCategoryUnavailable  = "not_available"
	ErrCategoryLimit        = "limit_reached"
	ErrCategoryUnknown      = "unknown"
)

var errCategoryMessages = map[string]string{
	ErrCategoryAccessDenied: "the cloud provider refused the purchase: CUDly's role is not allowed to make it in this account",
	ErrCategoryUnavailable:  "the requested commitment is not available from the provider",
	ErrCategoryLimit:        "a provider limit was reached",
	ErrCategoryUnknown:      "the provider returned an error",
}

// outcomeUnconfirmedMessage replaces the category message when the provider had
// already accepted the request and only the wait for its result failed.
const outcomeUnconfirmedMessage = "the provider accepted the request but its outcome could not be confirmed; check the account before trying again"

// postInsertWaitMarker is the cloud-commitments-go GCP wrapper around a failed
// wait on an operation whose Insert was accepted.
const postInsertWaitMarker = "commitment creation failed: "

const partialRunPrefix = "some purchases failed (partial success): "

// ErrCategoryMessage returns the fixed user message for a category.
func ErrCategoryMessage(category string) string {
	if m, ok := errCategoryMessages[category]; ok {
		return m
	}
	return errCategoryMessages[ErrCategoryUnknown]
}

// SummarizeProviderError classifies a live error by its type or explicit code
// fields and returns its category and fixed message.
func SummarizeProviderError(err error) (category, message string) {
	if err != nil && strings.Contains(err.Error(), postInsertWaitMarker) {
		return ErrCategoryUnknown, outcomeUnconfirmedMessage
	}
	category = classifyTypedError(err)
	if category == ErrCategoryUnknown && err != nil {
		category = classifyStoredText(err.Error())
	}
	return category, ErrCategoryMessage(category)
}

// SummarizeFailure is SummarizeProviderError for errors that may also be a
// known server-authored refusal: those keep their own readable text, since
// the category message would hide what to fix.
func SummarizeFailure(err error) (category, message string) {
	category, message = SummarizeProviderError(err)
	if category == ErrCategoryUnknown {
		if text, ok := ServerNoteMessage(err); ok {
			message = text
		}
	}
	return category, message
}

func classifyTypedError(err error) string {
	var apiErr interface{ ErrorCode() string }
	var azErr *azcore.ResponseError
	var gErr *googleapi.Error
	var authErr *azidentity.AuthenticationFailedError
	switch {
	case errors.As(err, &authErr):
		return ErrCategoryAccessDenied
	case errors.As(err, &apiErr):
		if c := classifyCode(apiErr.ErrorCode()); c != ErrCategoryUnknown {
			return c
		}
	case errors.As(err, &azErr):
		if c := classifyCode(azErr.ErrorCode); c != ErrCategoryUnknown {
			return c
		}
		return categoryForStatus(azErr.StatusCode)
	case errors.As(err, &gErr):
		for _, item := range gErr.Errors {
			if c := classifyCode(item.Reason); c != ErrCategoryUnknown {
				return c
			}
		}
		return categoryForStatus(gErr.Code)
	}
	var httpErr interface{ HTTPStatusCode() int }
	if errors.As(err, &httpErr) {
		return categoryForStatus(httpErr.HTTPStatusCode())
	}
	return ErrCategoryUnknown
}

func categoryForStatus(code int) string {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrCategoryAccessDenied
	case http.StatusNotFound:
		return ErrCategoryUnavailable
	case http.StatusTooManyRequests:
		return ErrCategoryLimit
	}
	return ErrCategoryUnknown
}

var (
	deniedCodes  = []string{"accessdenied", "unauthorized", "authorizationfailed", "forbidden"}
	limitCodes   = []string{"limitexceeded", "quota", "throttl", "toomanyrequests", "ratelimit"}
	unavailCodes = []string{"capacity", "offeringnotfound", "notfound", "notavailable"}
)

func containsAny(lower string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(lower, n) {
			return true
		}
	}
	return false
}

// classifyCode classifies one short error code or reason, never free text.
func classifyCode(code string) string {
	lower := strings.ToLower(code)
	switch {
	case containsAny(lower, deniedCodes):
		return ErrCategoryAccessDenied
	case containsAny(lower, limitCodes):
		return ErrCategoryLimit
	case containsAny(lower, unavailCodes):
		return ErrCategoryUnavailable
	}
	return ErrCategoryUnknown
}

// Explicit code fields in the providers' own error formats. Words elsewhere
// (URLs, resource names, bodies) never count.
var (
	awsCodeRE     = regexp.MustCompile(`api error ([A-Za-z0-9_.]+):`)
	awsStatusRE   = regexp.MustCompile(`StatusCode: (\d{3})`)
	azCodeRE      = regexp.MustCompile(`ERROR CODE: ([A-Za-z0-9_.-]+)`)
	azStatusRE    = regexp.MustCompile(`RESPONSE (\d{3}):`)
	gcpReasonRE   = regexp.MustCompile(`Reason: ([A-Za-z]+), Message:`)
	gcpErrorRE    = regexp.MustCompile(`googleapi: Error (\d{3}): [^\n]*?(?:, ([A-Za-z]+))?(?:\n|$)`)
	definiteAWSRE = regexp.MustCompile(`^(?:[A-Za-z0-9][A-Za-z0-9._-]*: )?operation error [^\n]*StatusCode: (4\d{2}),[^\n]*api error ([A-Za-z0-9_.]+): [^\n]*$`)
)

func statusOf(re *regexp.Regexp, s string) int {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n := 0
	for _, c := range m[1] {
		n = n*10 + int(c-'0')
	}
	return n
}

// classifyStoredText classifies a stored error string from its explicit code
// fields only; no code field means unknown.
func classifyStoredText(s string) string {
	if strings.Contains(s, postInsertWaitMarker) {
		return ErrCategoryUnknown
	}
	for _, re := range []*regexp.Regexp{awsCodeRE, azCodeRE, gcpErrorRE, gcpReasonRE} {
		for _, code := range lastGroups(re, s) {
			if c := classifyCode(code); c != ErrCategoryUnknown {
				return c
			}
		}
	}
	for _, re := range []*regexp.Regexp{awsStatusRE, azStatusRE, gcpErrorRE} {
		if c := categoryForStatus(statusOf(re, s)); c != ErrCategoryUnknown {
			return c
		}
	}
	return ErrCategoryUnknown
}

// lastGroups returns the last capture group of every match of re in s.
func lastGroups(re *regexp.Regexp, s string) []string {
	matches := re.FindAllStringSubmatch(s, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[len(m)-1])
	}
	return out
}

// definiteAWSCodes are the AWS error codes returned before anything is
// created: refused credentials, throttling and rejected parameters.
var definiteAWSCodes = map[string]bool{
	"AccessDenied": true, "AccessDeniedException": true, "UnauthorizedOperation": true, "AuthFailure": true,
	"UnrecognizedClientException": true, "InvalidClientTokenId": true, "ExpiredToken": true, "ExpiredTokenException": true,
	"Throttling": true, "ThrottlingException": true, "RequestLimitExceeded": true, "TooManyRequestsException": true,
	"LimitExceeded": true, "ReservedInstanceLimitExceeded": true, "ReservedInstancesLimitExceeded": true,
	"ReservedDBInstanceQuotaExceeded": true, "ReservedCacheNodeQuotaExceeded": true,
	"InvalidParameterValue": true, "InvalidParameterCombination": true, "InvalidParameter": true,
	"ValidationError": true, "ValidationException": true, "MissingParameter": true,
	"InvalidReservedInstancesOfferingId": true, "ReservedInstancesOfferingNotFound": true,
	"ReservedDBInstancesOfferingNotFound": true, "ReservedCacheNodesOfferingNotFound": true,
}

func definiteAWSSegment(seg string) bool {
	if strings.Contains(seg, "outcome unknown") {
		return false
	}
	m := definiteAWSRE.FindStringSubmatch(seg)
	return len(m) == 3 && definiteAWSCodes[m[2]]
}

// IsDefiniteAWSRefusal reports whether a stored error proves nothing was
// bought: every rec is AWS and every "; "-separated segment is an AWS 4xx
// with a listed pre-send code. Azure, GCP, 5xx, a missing code or a server
// note anywhere means the outcome is not proven, so the answer is false.
func IsDefiniteAWSRefusal(recs []config.RecommendationRecord, stored string) bool {
	if len(recs) == 0 {
		return false
	}
	for i := range recs {
		if recs[i].Provider != "" && recs[i].Provider != "aws" {
			return false
		}
	}
	stored = strings.TrimPrefix(stored, partialRunPrefix)
	for _, seg := range strings.Split(stored, "; ") {
		if !definiteAWSSegment(seg) {
			return false
		}
	}
	return stored != ""
}

// SanitizeStoredError turns a stored error string into text safe to show: it
// passes through only when every item is an allowlisted server note, and
// otherwise the whole remainder becomes one category message. The partial-run
// prefix is kept.
func SanitizeStoredError(s string) string {
	rest := strings.TrimPrefix(s, partialRunPrefix)
	prefix := s[:len(s)-len(rest)]
	if out, ok := renderServerNotes(rest); ok {
		return prefix + out
	}
	if strings.Contains(rest, postInsertWaitMarker) {
		return prefix + outcomeUnconfirmedMessage
	}
	return prefix + ErrCategoryMessage(classifyStoredText(rest))
}
