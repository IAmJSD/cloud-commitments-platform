package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// Opt-in cursor pagination for GET /api/recommendations (refs issue #723).
//
// The list endpoint used to return the whole filtered cache in one body,
// which passes the 6 MB Lambda response cap once a tenant accumulates a few
// thousand recommendations. A caller that sends `limit` (and, for later
// pages, the `cursor` returned as `next_cursor`) gets one bounded page.
// A paginated request reads only the page (plus one row) from the store, with
// the summary totaled by an SQL aggregate (issue #981, see
// handler_recommendations_scan.go). A page is bounded twice: by row count (limit) and by serialized bytes
// (recommendationsPageByteBudget), because the opaque Details blob makes the
// size of a row data-dependent.
// A request carrying neither parameter keeps the existing unpaginated shape
// unchanged, so the current frontend (which sorts, groups and totals the
// full set client-side) keeps working until it adopts pagination.
//
// In both modes `summary` and `regions` describe the full filtered set, not
// just the returned page, so headline totals stay correct while paging.
const (
	// DefaultRecommendationsPageSize is the page size used when a caller
	// sends a cursor without a limit.
	DefaultRecommendationsPageSize = 500

	// MaxRecommendationsPageSize caps the row count of one page. It is an
	// upper bound only: recommendationsPageByteBudget can cut a page shorter.
	MaxRecommendationsPageSize = 1000

	// recommendationsPageByteBudget caps the bytes one page adds to the
	// Lambda response payload. The Lambda limit (6 MiB, 6291456 bytes)
	// applies to the whole invoke response: the JSON envelope with status
	// code and headers, plus the body re-encoded as a JSON string, which
	// escapes every quote and backslash again. Rows are therefore measured
	// as they appear inside that string (see envelopeLen), and the budget is
	// checked against uncompressed bytes because the #775 gzip only runs
	// when the client sends Accept-Encoding: gzip. 4 MiB leaves about 2 MiB
	// for the envelope, headers and next_cursor, which are a few KiB.
	recommendationsPageByteBudget = 4 << 20

	// maxRecommendationsCursorLen bounds the encoded cursor so a hostile
	// query string cannot force a large decode.
	maxRecommendationsCursorLen = 1024
)

// recommendationsPage is the parsed pagination request. enabled is false when
// the caller sent neither limit nor cursor (legacy unpaginated mode).
type recommendationsPage struct {
	enabled bool
	limit   int
	afterID string
}

// parseRecommendationsPage validates the optional limit and cursor query
// parameters. limit must be an integer in 1..MaxRecommendationsPageSize;
// out-of-range values are rejected rather than clamped so a client bug does
// not silently return a different page size than it asked for. Legacy
// unpaginated mode is selected only when neither parameter is present: a
// present but empty value (?limit= or ?cursor=) is a 400, so a client bug
// cannot silently fall back to the unbounded response.
func parseRecommendationsPage(params map[string]string) (recommendationsPage, error) {
	rawLimit, hasLimit := params["limit"]
	rawCursor, hasCursor := params["cursor"]
	if !hasLimit && !hasCursor {
		return recommendationsPage{}, nil
	}
	// From here a present but empty value fails the checks below: "" is not
	// an integer, and an empty cursor does not decode to an ID.
	rawLimit = strings.TrimSpace(rawLimit)
	rawCursor = strings.TrimSpace(rawCursor)

	page := recommendationsPage{enabled: true, limit: DefaultRecommendationsPageSize}
	if hasLimit {
		n, err := strconv.Atoi(rawLimit)
		if err != nil || n < 1 || n > MaxRecommendationsPageSize {
			return recommendationsPage{}, NewClientError(400,
				"invalid limit: must be an integer between 1 and "+strconv.Itoa(MaxRecommendationsPageSize))
		}
		page.limit = n
	}
	if hasCursor {
		afterID, err := decodeRecommendationsCursor(rawCursor)
		if err != nil {
			return recommendationsPage{}, err
		}
		page.afterID = afterID
	}
	return page, nil
}

// encodeRecommendationsCursor turns the last row ID of a page into an opaque,
// URL-safe cursor. Recommendation IDs can carry characters such as "|", so
// the raw ID is not used directly in the query string.
func encodeRecommendationsCursor(id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

// decodeRecommendationsCursor reverses encodeRecommendationsCursor and
// rejects malformed or oversized cursors with a 400.
func decodeRecommendationsCursor(cursor string) (string, error) {
	if len(cursor) > maxRecommendationsCursorLen {
		return "", NewClientError(400, "invalid cursor: too long")
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(raw) == 0 {
		return "", NewClientError(400, "invalid cursor")
	}
	return string(raw), nil
}

// envelopeLen returns how many bytes v adds to the Lambda invoke response:
// its JSON encoding, re-encoded as the content of the response's body
// string (without the surrounding quotes).
func envelopeLen(v any) (int, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	quoted, err := json.Marshal(string(raw))
	if err != nil {
		return 0, err
	}
	return len(quoted) - 2, nil
}

// selectRecommendationsPage stamps and measures rows from sorted[start:] and
// returns how many fit in one page: at most limit rows, and no more bytes
// than budget once the fixed overhead (summary, regions, keys) and the next
// cursor are counted. It always takes at least one row when any remain, so a
// row that alone exceeds the budget cannot stall the cursor walk; that row
// is returned on its own page with a logged warning (it only fails at Lambda
// if it is over the 6 MiB hard cap by itself, which no page size could fix).
func selectRecommendationsPage(sorted []config.RecommendationRecord, start, limit, overhead, budget int) (int, error) {
	used := overhead
	n := 0
	for i := start; i < len(sorted) && n < limit; i++ {
		stampComputeCapacity(&sorted[i])
		size, err := envelopeLen(&sorted[i])
		if err != nil {
			return 0, fmt.Errorf("failed to size recommendation %q: %w", sorted[i].ID, err)
		}
		// +1 for the separating comma; the cursor that would point at this
		// row is reserved so next_cursor always fits.
		cost := size + 1
		cursorLen := base64.RawURLEncoding.EncodedLen(len(sorted[i].ID))
		if n > 0 && used+cost+cursorLen > budget {
			break
		}
		if n == 0 && used+cost+cursorLen > budget {
			logging.Warnf("recommendation %q alone is %d bytes, over the %d-byte page budget; returning it on its own page", sorted[i].ID, size, budget)
		}
		used += cost
		n++
	}
	return n, nil
}
