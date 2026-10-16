package api

import (
	"encoding/base64"
	"sort"
	"strconv"
	"strings"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// Opt-in keyset pagination for GET /api/recommendations (issue #723).
//
// The list endpoint used to return the whole filtered cache in one body,
// which passes the 6 MB Lambda response cap once a tenant accumulates a few
// thousand recommendations. A caller that sends `limit` (and, for later
// pages, the `cursor` returned as `next_cursor`) gets one bounded page.
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

	// MaxRecommendationsPageSize caps one page. List rows are roughly
	// 500-600 bytes of JSON, so a full page stays far below the 6 MB cap.
	MaxRecommendationsPageSize = 1000

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
// not silently return a different page size than it asked for.
func parseRecommendationsPage(params map[string]string) (recommendationsPage, error) {
	rawLimit := strings.TrimSpace(params["limit"])
	rawCursor := strings.TrimSpace(params["cursor"])
	if rawLimit == "" && rawCursor == "" {
		return recommendationsPage{}, nil
	}

	page := recommendationsPage{enabled: true, limit: DefaultRecommendationsPageSize}
	if rawLimit != "" {
		n, err := strconv.Atoi(rawLimit)
		if err != nil || n < 1 || n > MaxRecommendationsPageSize {
			return recommendationsPage{}, NewClientError(400,
				"invalid limit: must be an integer between 1 and "+strconv.Itoa(MaxRecommendationsPageSize))
		}
		page.limit = n
	}
	if rawCursor != "" {
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

// paginateRecommendations orders the filtered set by ID and returns the rows
// strictly after page.afterID, at most page.limit of them. recs is sorted in
// place (it is this request's own slice, freshly read from the store) and
// returned as sorted; rows is a sub-slice of it. nextCursor is empty on the
// last page.
//
// The ID is the cache table's primary key, so ID order is a total order and
// no row can be skipped or repeated at a page boundary.
//
// Keyset (rather than offset) paging means a row added or removed between
// two requests does not shift later pages: the next page always resumes
// after the last ID the client saw, even if that row has since been deleted.
func paginateRecommendations(recs []config.RecommendationRecord, page recommendationsPage) (sorted, rows []config.RecommendationRecord, nextCursor string) {
	sorted = recs
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	start := 0
	if page.afterID != "" {
		start = sort.Search(len(sorted), func(i int) bool { return sorted[i].ID > page.afterID })
	}
	end := min(start+page.limit, len(sorted))
	rows = sorted[start:end]
	if end < len(sorted) {
		nextCursor = encodeRecommendationsCursor(sorted[end-1].ID)
	}
	return sorted, rows, nextCursor
}

// buildRecommendationsPageResponse builds the list response for either mode.
// Unpaginated requests get exactly the legacy buildRecommendationsResponse
// output; paginated requests get one page of rows plus next_cursor, with the
// summary and regions still computed over the full filtered set.
func buildRecommendationsPageResponse(recs []config.RecommendationRecord, page recommendationsPage) *RecommendationsResponse {
	if !page.enabled {
		return buildRecommendationsResponse(recs)
	}
	sorted, rows, next := paginateRecommendations(recs, page)
	resp := summarizeRecommendations(sorted, rows)
	resp.NextCursor = next
	return resp
}
