package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/insurance"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/aws/aws-lambda-go/events"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/archera"
)

// insuranceTimeoutMessage is the 504 text for a request whose deadline ended.
const insuranceTimeoutMessage = "Archera comparison timed out"

// maxComparisonResponseBytes bounds the JSON returned to the browser. A larger
// comparison fails loudly rather than dropping rows.
const maxComparisonResponseBytes = 5 << 20

// getInsuranceComparison runs the explicit, read-only Archera comparison for
// the configured plan. Same gate as the status route. Only mapped, fixed-text
// errors leave this function; vendor text and the key never reach the response
// or the log.
func (h *Handler) getInsuranceComparison(ctx context.Context, req *events.LambdaFunctionURLRequest) (*archera.ComparisonDTO, error) {
	if err := h.requireInsuranceAccess(ctx, req); err != nil {
		return nil, err
	}
	client, err := h.insuranceClient(ctx)
	if err != nil {
		return nil, err
	}
	cmp, err := client.Comparison(ctx, insurance.ComparisonRequest{PlanID: h.insurance.PlanID()})
	if err != nil {
		if isContextEnded(err) {
			return nil, NewClientError(504, insuranceTimeoutMessage)
		}
		return nil, mapInsuranceError(err)
	}
	dto, err := archera.BuildComparison(cmp)
	if err != nil {
		logging.Warnf("archera comparison: a vendor value has no exact decimal form")
		return nil, NewClientError(502, "Archera returned a value that cannot be shown exactly")
	}
	raw, err := json.Marshal(dto)
	if err != nil || len(raw) > maxComparisonResponseBytes {
		return nil, NewClientError(502, "comparison too large to return")
	}
	return dto, nil
}

// insuranceClient resolves the Archera client, or a fixed client error. The raw
// client lives in the caller's local only.
func (h *Handler) insuranceClient(ctx context.Context) (insurance.QuoteClient, error) {
	if h.insurance == nil {
		return nil, notConfiguredError(archera.Settings{}.Missing())
	}
	if st := h.insurance.Status(); !st.Configured {
		return nil, notConfiguredError(st.Missing)
	}
	client, err := h.insurance.Client(ctx)
	if err != nil {
		if errors.Is(err, archera.ErrNotConfigured) {
			return nil, notConfiguredError(h.insurance.Status().Missing)
		}
		if isContextEnded(err) {
			return nil, NewClientError(504, insuranceTimeoutMessage)
		}
		return nil, NewClientError(503, "Archera API key could not be resolved from "+archera.EnvKeySecret)
	}
	return client, nil
}

// isContextEnded reports whether err carries a context deadline or
// cancellation. It is answered with the 504 timeout status, not as a vendor or
// credential failure, whether it ends the key build or the vendor call (issue
// #1498).
func isContextEnded(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func notConfiguredError(missing []string) error {
	return NewClientError(503, "Archera comparison is not configured; missing settings: "+strings.Join(missing, ", "))
}

// mapInsuranceError converts a pkg/insurance error to a fixed client error.
// Only the status code and Retry-After are logged; HTTPError.Message is vendor
// text and is never returned or logged.
func mapInsuranceError(err error) error {
	var he *insurance.HTTPError
	if !errors.As(err, &he) {
		logging.Warnf("archera comparison failed: non-HTTP error")
		return NewClientError(502, "Archera comparison failed")
	}
	logging.Warnf("archera comparison failed: http_status=%d retry_after=%s", he.StatusCode, he.RetryAfter)
	switch {
	case he.StatusCode == 401 || he.StatusCode == 403:
		return NewClientError(502, "Archera rejected the configured credentials")
	case he.StatusCode == 404:
		return NewClientError(502, "Archera organization or plan was not found")
	case he.StatusCode == 429:
		if he.RetryAfter <= 0 {
			return NewClientError(429, "Archera rate limit reached; retry-after not given")
		}
		secs := int((he.RetryAfter + time.Second - 1) / time.Second)
		return NewClientErrorWithHeaders(429, "Archera rate limit reached",
			map[string]any{"retry_after_seconds": secs},
			map[string]string{"Retry-After": strconv.Itoa(secs)})
	case he.StatusCode >= 500:
		return NewClientError(502, "Archera service error")
	}
	return NewClientError(502, fmt.Sprintf("Archera request failed with HTTP %d", he.StatusCode))
}
