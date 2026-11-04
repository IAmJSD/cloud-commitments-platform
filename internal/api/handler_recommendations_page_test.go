package api

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// lambdaResponseCapBytes is the AWS Lambda synchronous response payload cap
// that GET /api/recommendations used to exceed (issue #723).
const lambdaResponseCapBytes = 6 * 1024 * 1024

// pageTestRecs returns n recommendations in deliberately non-ID order, each
// padded so a single row is about 1 KB of JSON.
func pageTestRecs(n int) []config.RecommendationRecord {
	pad := strings.Repeat("x", 900)
	recs := make([]config.RecommendationRecord, n)
	for i := range n {
		// Reverse order so the handler has to sort to page stably.
		idx := n - 1 - i
		recs[i] = config.RecommendationRecord{
			ID:           fmt.Sprintf("rec-%06d", idx),
			Provider:     "aws",
			Service:      "ec2",
			Region:       "us-east-" + strconv.Itoa(idx%3+1),
			ResourceType: "m5.large-" + pad,
			Savings:      10,
			UpfrontCost:  20,
		}
	}
	return recs
}

// pageTestHandler serves recs through the read each request mode uses:
// unpaginated requests read the full list, paginated ones scan it in ID order
// (issue #981). list and scan say which reads the test's requests must make.
func pageTestHandler(t *testing.T, recs []config.RecommendationRecord, list, scan bool) *Handler {
	t.Helper()
	ms := new(MockScheduler)
	t.Cleanup(func() { ms.AssertExpectations(t) })
	if list {
		ms.On("ListRecommendations", mock.Anything, mock.Anything).Return(recs, nil)
	}
	if scan {
		ms.On("ScanRecommendations", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(recs, nil)
	}
	return &Handler{scheduler: ms, apiKey: "test-key"}
}

// buildBudgetedPage pages recs the way a paginated request does: the rows
// reach the collector in ID order, as the store scan delivers them, and the
// page is cut with the given byte budget.
func buildBudgetedPage(recs []config.RecommendationRecord, page recommendationsPage, budget int) (*RecommendationsResponse, error) {
	sorted := append([]config.RecommendationRecord(nil), recs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	collector := newRecommendationsPageCollector(page)
	if err := collector.add(sorted); err != nil {
		return nil, err
	}
	return collector.response(budget)
}

func pageTestReq() *events.LambdaFunctionURLRequest {
	return &events.LambdaFunctionURLRequest{Headers: map[string]string{"x-api-key": "test-key"}}
}

// TestGetRecommendations_PaginationBoundsResponse is the #723 regression: a
// tenant whose full list is well over 6 MB must be able to fetch it in pages
// that each fit under the Lambda cap, and walking the cursor must return
// every row exactly once.
func TestGetRecommendations_PaginationBoundsResponse(t *testing.T) {
	const total = 8000
	recs := pageTestRecs(total)
	h := pageTestHandler(t, recs, false, true)

	full, err := json.Marshal(buildRecommendationsResponse(append([]config.RecommendationRecord(nil), recs...)))
	require.NoError(t, err)
	require.Greater(t, len(full), lambdaResponseCapBytes, "fixture must exceed the cap to be meaningful")

	seen := make(map[string]int, total)
	var order []string
	params := map[string]string{"limit": strconv.Itoa(MaxRecommendationsPageSize)}
	for pages := 0; ; pages++ {
		require.Less(t, pages, total, "cursor walk did not terminate")
		resp, err := h.getRecommendations(context.Background(), pageTestReq(), params)
		require.NoError(t, err)

		body, err := json.Marshal(resp)
		require.NoError(t, err)
		assert.Less(t, len(body), lambdaResponseCapBytes, "page %d exceeds the Lambda response cap", pages)
		assert.LessOrEqual(t, len(resp.Recommendations), MaxRecommendationsPageSize)

		// Summary describes the full filtered set on every page.
		assert.Equal(t, total, resp.Summary.TotalCount)
		assert.InDelta(t, float64(total*10), resp.Summary.TotalMonthlySavings, 1e-6)
		assert.Len(t, resp.Regions, 3)

		for i := range resp.Recommendations {
			id := resp.Recommendations[i].ID
			seen[id]++
			order = append(order, id)
		}
		if resp.NextCursor == "" {
			break
		}
		params = map[string]string{"limit": strconv.Itoa(MaxRecommendationsPageSize), "cursor": resp.NextCursor}
	}

	assert.Len(t, seen, total, "every row must be returned")
	for id, n := range seen {
		assert.Equal(t, 1, n, "row %s returned %d times", id, n)
	}
	assert.IsIncreasing(t, order, "rows must be returned in ID order across pages")
}

// TestGetRecommendations_UnpaginatedShapeUnchanged pins backward
// compatibility: without limit/cursor the endpoint still returns every row,
// in the order the scheduler gave them, with no next_cursor.
func TestGetRecommendations_UnpaginatedShapeUnchanged(t *testing.T) {
	recs := pageTestRecs(5)
	// Copy the expected order before the call: the mock hands the handler
	// this same backing array, so an in-place sort would also reorder recs.
	wantIDs := make([]string, len(recs))
	for i := range recs {
		wantIDs[i] = recs[i].ID
	}
	h := pageTestHandler(t, recs, true, false)

	resp, err := h.getRecommendations(context.Background(), pageTestReq(), map[string]string{})
	require.NoError(t, err)
	require.Len(t, resp.Recommendations, 5)
	gotIDs := make([]string, len(resp.Recommendations))
	for i := range resp.Recommendations {
		gotIDs[i] = resp.Recommendations[i].ID
	}
	assert.Equal(t, wantIDs, gotIDs, "legacy mode must keep the scheduler's order")
	assert.Empty(t, resp.NextCursor)
	assert.Equal(t, 5, resp.Summary.TotalCount)

	body, err := json.Marshal(resp)
	require.NoError(t, err)
	assert.NotContains(t, string(body), "next_cursor")
}

// TestGetRecommendations_PaginationRejectsDuplicateIDs pins the total-order
// guard: two rows sharing an ID would let the strict "after" comparison skip
// one of them at a page boundary (a,b,b,c with limit=2 used to return only
// three rows), so a paginated request fails instead of walking short. Legacy
// mode does not sort or page and still returns every row.
func TestGetRecommendations_PaginationRejectsDuplicateIDs(t *testing.T) {
	recs := pageTestRecs(4)
	recs[1].ID = recs[2].ID
	h := pageTestHandler(t, recs, true, true)
	ctx := context.Background()

	_, err := h.getRecommendations(ctx, pageTestReq(), map[string]string{"limit": "2"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate recommendation ID")

	legacy, err := h.getRecommendations(ctx, pageTestReq(), map[string]string{})
	require.NoError(t, err)
	assert.Len(t, legacy.Recommendations, 4)
}

func TestGetRecommendations_PaginationPages(t *testing.T) {
	recs := pageTestRecs(5) // rec-000000 .. rec-000004, stored in reverse
	h := pageTestHandler(t, recs, false, true)
	ctx := context.Background()

	first, err := h.getRecommendations(ctx, pageTestReq(), map[string]string{"limit": "2"})
	require.NoError(t, err)
	require.Len(t, first.Recommendations, 2)
	assert.Equal(t, "rec-000000", first.Recommendations[0].ID)
	assert.Equal(t, "rec-000001", first.Recommendations[1].ID)
	assert.Equal(t, encodeRecommendationsCursor("rec-000001"), first.NextCursor)
	assert.Equal(t, 5, first.Summary.TotalCount)

	last, err := h.getRecommendations(ctx, pageTestReq(), map[string]string{
		"limit": "2", "cursor": encodeRecommendationsCursor("rec-000003"),
	})
	require.NoError(t, err)
	require.Len(t, last.Recommendations, 1)
	assert.Equal(t, "rec-000004", last.Recommendations[0].ID)
	assert.Empty(t, last.NextCursor, "last page must not carry a cursor")

	// Exact fit: the page ending on the final row has no next cursor.
	exact, err := h.getRecommendations(ctx, pageTestReq(), map[string]string{"limit": "5"})
	require.NoError(t, err)
	assert.Len(t, exact.Recommendations, 5)
	assert.Empty(t, exact.NextCursor)

	// Keyset: a cursor naming a row that has since disappeared resumes at
	// the next ID instead of failing or restarting.
	gone, err := h.getRecommendations(ctx, pageTestReq(), map[string]string{
		"limit": "2", "cursor": encodeRecommendationsCursor("rec-000001x"),
	})
	require.NoError(t, err)
	require.Len(t, gone.Recommendations, 2)
	assert.Equal(t, "rec-000002", gone.Recommendations[0].ID)

	// Cursor past the end yields an empty page, not an error.
	past, err := h.getRecommendations(ctx, pageTestReq(), map[string]string{
		"cursor": encodeRecommendationsCursor("rec-999999"),
	})
	require.NoError(t, err)
	assert.Empty(t, past.Recommendations)
	assert.Empty(t, past.NextCursor)
	assert.Equal(t, 5, past.Summary.TotalCount)
}

func TestGetRecommendations_CursorWithoutLimitUsesDefaultPageSize(t *testing.T) {
	recs := pageTestRecs(DefaultRecommendationsPageSize + 3)
	h := pageTestHandler(t, recs, false, true)

	resp, err := h.getRecommendations(context.Background(), pageTestReq(), map[string]string{
		"cursor": encodeRecommendationsCursor("rec-"),
	})
	require.NoError(t, err)
	assert.Len(t, resp.Recommendations, DefaultRecommendationsPageSize)
	assert.NotEmpty(t, resp.NextCursor)
}

func TestGetRecommendations_PaginationRejectsBadParams(t *testing.T) {
	cases := map[string]map[string]string{
		"zero limit":        {"limit": "0"},
		"negative limit":    {"limit": "-1"},
		"limit above max":   {"limit": strconv.Itoa(MaxRecommendationsPageSize + 1)},
		"non-numeric limit": {"limit": "abc"},
		"fractional limit":  {"limit": "1.5"},
		"non-base64 cursor": {"cursor": "!!!"},
		// Valid base64 (length is a multiple of 4), so only the length cap
		// can reject it.
		"oversized cursor":   {"cursor": strings.Repeat("A", maxRecommendationsCursorLen+4)},
		"bad cursor + limit": {"limit": "10", "cursor": "%%%"},
		// Present but empty must not fall back to the unbounded legacy shape.
		"empty limit":                {"limit": ""},
		"blank limit":                {"limit": "   "},
		"empty cursor":               {"cursor": ""},
		"blank cursor":               {"cursor": " \t"},
		"valid limit + empty cursor": {"limit": "10", "cursor": ""},
		"empty limit + valid cursor": {"limit": "", "cursor": encodeRecommendationsCursor("rec-000001")},
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			// No scheduler: validation must reject before any store read.
			h := &Handler{apiKey: "test-key"}
			_, err := h.getRecommendations(context.Background(), pageTestReq(), params)
			require.Error(t, err)
			ce, ok := IsClientError(err)
			require.True(t, ok, "expected ClientError, got %T: %v", err, err)
			assert.Equal(t, 400, ce.code)
		})
	}
}

func TestGetRecommendations_PaginationMaxLimitAccepted(t *testing.T) {
	h := pageTestHandler(t, pageTestRecs(3), false, true)
	resp, err := h.getRecommendations(context.Background(), pageTestReq(), map[string]string{
		"limit": strconv.Itoa(MaxRecommendationsPageSize),
	})
	require.NoError(t, err)
	assert.Len(t, resp.Recommendations, 3)
	assert.Empty(t, resp.NextCursor)
}

// bigDetailsRecs returns n recommendations whose Details blob is detailBytes
// of quote-heavy JSON. Each quote doubles again when the body is re-encoded
// into the Lambda response envelope, so measuring raw JSON would undercount.
func bigDetailsRecs(n, detailBytes int) []config.RecommendationRecord {
	recs := pageTestRecs(n)
	note := strings.Repeat(`"<&\\`, detailBytes/4)
	details, err := json.Marshal(map[string]string{"note": note})
	if err != nil {
		panic(err)
	}
	for i := range recs {
		recs[i].ResourceType = "m5.large"
		recs[i].Details = details
	}
	return recs
}

// lambdaPayloadLen is the size of the Lambda invoke response that would carry
// resp, matching how handler.go and the Lambda runtime encode it.
func lambdaPayloadLen(t *testing.T, resp *RecommendationsResponse) (bodyInEnvelope, total int) {
	t.Helper()
	body, err := json.Marshal(resp)
	require.NoError(t, err)
	quoted, err := json.Marshal(string(body))
	require.NoError(t, err)
	envelope, err := json.Marshal(events.LambdaFunctionURLResponse{
		StatusCode: 200,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(body),
	})
	require.NoError(t, err)
	return len(quoted) - 2, len(envelope)
}

// TestGetRecommendations_PageCutByByteBudget: with large Details, limit rows
// would be far over the Lambda cap, so pages must be cut by serialized bytes
// while still visiting every row exactly once, in order, with full totals.
func TestGetRecommendations_PageCutByByteBudget(t *testing.T) {
	const total = 1500
	recs := bigDetailsRecs(total, 1500) // ~10 KB per row in the envelope
	h := pageTestHandler(t, recs, false, true)

	_, fullSize := lambdaPayloadLen(t, buildRecommendationsResponse(append([]config.RecommendationRecord(nil), recs...)))
	require.Greater(t, fullSize, 2*lambdaResponseCapBytes, "fixture must be far over the cap")

	seen := make(map[string]int, total)
	var order []string
	cutByBytes := false
	params := map[string]string{"limit": strconv.Itoa(MaxRecommendationsPageSize)}
	for pages := 0; ; pages++ {
		require.Less(t, pages, total, "cursor walk did not terminate")
		resp, err := h.getRecommendations(context.Background(), pageTestReq(), params)
		require.NoError(t, err)

		inEnvelope, payload := lambdaPayloadLen(t, resp)
		assert.LessOrEqual(t, inEnvelope, recommendationsPageByteBudget, "page %d over the byte budget", pages)
		assert.Less(t, payload, lambdaResponseCapBytes, "page %d over the Lambda cap", pages)
		require.NotEmpty(t, resp.Recommendations, "a page must never be empty mid-walk")
		if resp.NextCursor != "" && len(resp.Recommendations) < MaxRecommendationsPageSize {
			cutByBytes = true
		}

		assert.Equal(t, total, resp.Summary.TotalCount)
		assert.InDelta(t, float64(total*10), resp.Summary.TotalMonthlySavings, 1e-6)
		assert.InDelta(t, float64(total*20), resp.Summary.TotalUpfrontCost, 1e-6)

		for i := range resp.Recommendations {
			seen[resp.Recommendations[i].ID]++
			order = append(order, resp.Recommendations[i].ID)
		}
		if resp.NextCursor == "" {
			break
		}
		params = map[string]string{"limit": strconv.Itoa(MaxRecommendationsPageSize), "cursor": resp.NextCursor}
	}

	assert.True(t, cutByBytes, "expected at least one page cut by the byte budget before limit")
	assert.Len(t, seen, total)
	for id, n := range seen {
		assert.Equal(t, 1, n, "row %s returned %d times", id, n)
	}
	assert.IsIncreasing(t, order)
}

// TestBuildBudgetedPage_OversizedRowAlone: a row that alone exceeds the
// budget is returned on its own page so the walk advances, and the rows
// around it still pack normally.
func TestBuildBudgetedPage_OversizedRowAlone(t *testing.T) {
	recs := pageTestRecs(6) // ~1 KB rows: rec-000000 .. rec-000005
	huge := bigDetailsRecs(1, 8000)[0]
	recs[2].Details = huge.Details // rec-000003 (pageTestRecs is reversed)
	const budget = 4000            // fits a few small rows, never the big one

	var pages [][]string
	page := recommendationsPage{enabled: true, limit: MaxRecommendationsPageSize}
	for range len(recs) + 1 {
		resp, err := buildBudgetedPage(append([]config.RecommendationRecord(nil), recs...), page, budget)
		require.NoError(t, err)
		require.NotEmpty(t, resp.Recommendations)
		ids := make([]string, 0, len(resp.Recommendations))
		for i := range resp.Recommendations {
			ids = append(ids, resp.Recommendations[i].ID)
		}
		pages = append(pages, ids)
		if len(ids) > 1 {
			inEnvelope, _ := lambdaPayloadLen(t, resp)
			assert.LessOrEqual(t, inEnvelope, budget, "multi-row page over budget: %v", ids)
		}
		if resp.NextCursor == "" {
			break
		}
		cursor, err := decodeRecommendationsCursor(resp.NextCursor)
		require.NoError(t, err)
		page.afterID = cursor
	}

	assert.Contains(t, pages, []string{"rec-000003"}, "oversized row must be alone on its page")
	all := make([]string, 0, len(recs))
	for _, p := range pages {
		all = append(all, p...)
	}
	assert.Equal(t, []string{"rec-000000", "rec-000001", "rec-000002", "rec-000003", "rec-000004", "rec-000005"}, all)
	assert.Greater(t, len(pages[0]), 1, "small rows before the big one must still share a page")
}

// TestBuildBudgetedPage_TightBudgetSweep sweeps the budget one byte at a time
// so the accounting is checked at its exact edges: every multi-row page must
// fit (counting the summary, regions, keys and next_cursor), and the page
// must be maximal, i.e. one more row would not have fit.
func TestBuildBudgetedPage_TightBudgetSweep(t *testing.T) {
	recs := pageTestRecs(6)
	page := recommendationsPage{enabled: true, limit: MaxRecommendationsPageSize}
	for budget := 1500; budget <= 7000; budget++ {
		resp, err := buildBudgetedPage(append([]config.RecommendationRecord(nil), recs...), page, budget)
		require.NoError(t, err)
		n := len(resp.Recommendations)
		require.GreaterOrEqual(t, n, 1)
		if n > 1 {
			inEnvelope, _ := lambdaPayloadLen(t, resp)
			require.LessOrEqual(t, inEnvelope, budget, "budget %d: %d rows do not fit", budget, n)
		}
		if n < len(recs) {
			// The same page plus the next row, with that row's cursor.
			bigger, err := buildBudgetedPage(append([]config.RecommendationRecord(nil), recs...),
				recommendationsPage{enabled: true, limit: n + 1}, 1<<30)
			require.NoError(t, err)
			bigger.NextCursor = encodeRecommendationsCursor(bigger.Recommendations[n].ID)
			inEnvelope, _ := lambdaPayloadLen(t, bigger)
			// The paginator overcounts by at most 2 bytes (one comma and the
			// one-byte cursor placeholder in the overhead), so anything it
			// rejected must have been within 2 bytes of overflowing.
			require.Greater(t, inEnvelope+2, budget, "budget %d: page of %d rows is not maximal", budget, n)
		}
	}
}
