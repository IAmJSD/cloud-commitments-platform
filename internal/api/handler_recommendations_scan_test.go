package api

import (
	"context"
	"sort"
	"strconv"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/scheduler"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// chunkScanScheduler serves ScanRecommendations from recs like the store
// scan does (ID order, chunk rows per call) but with a chunk size the test
// picks, so page and chunk boundaries can be made to cross. Every other
// scheduler method goes to the embedded mock, which has no expectations: a
// paginated request that falls back to ListRecommendations panics.
type chunkScanScheduler struct {
	*MockScheduler
	recs  []config.RecommendationRecord
	chunk int
	ctxs  []context.Context
}

func (s *chunkScanScheduler) ScanRecommendations(ctx context.Context, _ config.RecommendationFilter, _ int, fn func([]config.RecommendationRecord) error) error {
	s.ctxs = append(s.ctxs, ctx)
	sorted := append([]config.RecommendationRecord(nil), s.recs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for start := 0; start < len(sorted); start += s.chunk {
		end := min(start+s.chunk, len(sorted))
		// A fresh slice per chunk, as the store returns.
		if err := fn(append([]config.RecommendationRecord(nil), sorted[start:end]...)); err != nil {
			return err
		}
	}
	return nil
}

// scanFilterRecs mixes IDs that a linguistic collation orders differently
// from bytes with rows the handler-side filters drop: an Azure row (only AWS
// is enabled in recsReadStore), a row in an account outside the scoped
// user's scope and a row with no account.
func scanFilterRecs() []config.RecommendationRecord {
	in, out := scopedInAccount, "22222222-2222-4222-8222-222222222222"
	mk := func(id, provider string, account *string, region string, savings float64) config.RecommendationRecord {
		return config.RecommendationRecord{
			ID: id, Provider: provider, Service: "ec2", Region: region, ResourceType: "m5.large",
			CloudAccountID: account, Savings: savings, UpfrontCost: 2 * savings,
		}
	}
	return []config.RecommendationRecord{
		mk("aws|b", "aws", &in, "us-east-1", 1),
		mk("AWS|a", "aws", &in, "us-east-2", 2),
		mk("aws-a", "aws", &in, "eu-west-1", 3),
		mk("aws a", "aws", &in, "us-east-1", 4),
		mk("azure|x", "azure", &in, "westeurope", 5),
		mk("aws/a", "aws", &out, "ap-south-1", 6),
		mk("aws|a", "aws", &in, "us-east-1", 7),
		mk("Zeta", "aws", &in, "us-west-2", 8),
		mk("ambient", "aws", nil, "us-east-1", 9),
		mk("élan", "aws", &in, "us-east-1", 10),
		mk("elan", "aws", &in, "sa-east-1", 11),
	}
}

func scanTestHandler(recs []config.RecommendationRecord, chunk int, scoped bool) (*Handler, *chunkScanScheduler) {
	sched := &chunkScanScheduler{MockScheduler: new(MockScheduler), recs: recs, chunk: chunk}
	accounts := []string(nil)
	if scoped {
		accounts = []string{scopedInAccount}
	}
	return &Handler{config: recsReadStore(), scheduler: sched, auth: scopedAuth(accounts)}, sched
}

// legacyResponse is the unpaginated response for recs, read through
// ListRecommendations on a separate handler with its own copy (the legacy
// scope filter compacts its input in place).
func legacyResponse(t *testing.T, recs []config.RecommendationRecord, scoped bool) *RecommendationsResponse {
	t.Helper()
	ms := new(MockScheduler)
	t.Cleanup(func() { ms.AssertExpectations(t) })
	ms.On("ListRecommendations", mock.Anything, mock.Anything).
		Return(append([]config.RecommendationRecord(nil), recs...), nil)
	accounts := []string(nil)
	if scoped {
		accounts = []string{scopedInAccount}
	}
	h := &Handler{config: recsReadStore(), scheduler: ms, auth: scopedAuth(accounts)}
	resp, err := h.getRecommendations(context.Background(), scopedRequest(""), map[string]string{})
	require.NoError(t, err)
	return resp
}

// TestGetRecommendations_PaginatedRequestDoesNotLoadFullList is the #981
// regression: a paginated request must page from the chunked scan and never
// read the whole filtered set through ListRecommendations.
func TestGetRecommendations_PaginatedRequestDoesNotLoadFullList(t *testing.T) {
	recs := pageTestRecs(5)
	ms := new(MockScheduler)
	t.Cleanup(func() { ms.AssertExpectations(t) })
	ms.On("ScanRecommendations", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(recs, nil)
	h := &Handler{scheduler: ms, apiKey: "test-key"}

	resp, err := h.getRecommendations(context.Background(), pageTestReq(), map[string]string{"limit": "2"})
	require.NoError(t, err)
	require.Len(t, resp.Recommendations, 2)
	assert.Equal(t, 5, resp.Summary.TotalCount)
	ms.AssertNotCalled(t, "ListRecommendations", mock.Anything, mock.Anything)
	ms.AssertCalled(t, "ScanRecommendations", mock.Anything, mock.Anything, recommendationsScanChunkSize, mock.Anything)
}

// TestGetRecommendations_PagedWalkMatchesLegacyOrder pins the paginated
// output to what the in-memory paging returned: the legacy (unpaginated) rows
// after every filter, sorted by Go string order, cut into consecutive pages
// of limit rows, with next_cursor on every page but the last and the
// full-set summary on every page. It runs for an unrestricted and a scoped
// reader, across chunk sizes that put chunk boundaries before, on and after
// page boundaries, including the exact-fit last page.
func TestGetRecommendations_PagedWalkMatchesLegacyOrder(t *testing.T) {
	recs := scanFilterRecs()
	for _, scoped := range []bool{false, true} {
		legacy := legacyResponse(t, recs, scoped)
		want := make([]string, len(legacy.Recommendations))
		for i := range legacy.Recommendations {
			want[i] = legacy.Recommendations[i].ID
		}
		sort.Strings(want)
		require.NotEmpty(t, want)
		if scoped {
			require.Len(t, want, 8, "fixture: Azure, out-of-scope and account-less rows dropped")
		}
		n := len(want)
		for _, chunk := range []int{1, 2, 3, n, n + 1} {
			for _, limit := range []int{1, 2, 3, n - 1, n, n + 1} {
				t.Run("scoped="+strconv.FormatBool(scoped)+"/chunk="+strconv.Itoa(chunk)+"/limit="+strconv.Itoa(limit), func(t *testing.T) {
					h, _ := scanTestHandler(recs, chunk, scoped)
					var got []string
					params := map[string]string{"limit": strconv.Itoa(limit)}
					for page := 0; ; page++ {
						require.LessOrEqual(t, page, n, "cursor walk did not terminate")
						resp, err := h.getRecommendations(context.Background(), scopedRequest(""), params)
						require.NoError(t, err)

						start := page * limit
						end := min(start+limit, n)
						ids := make([]string, len(resp.Recommendations))
						for i := range resp.Recommendations {
							ids[i] = resp.Recommendations[i].ID
						}
						require.Equal(t, want[start:end], ids, "page %d", page)
						assert.Equal(t, legacy.Summary, resp.Summary, "summary must cover the full filtered set")
						assert.ElementsMatch(t, legacy.Regions, resp.Regions)
						got = append(got, ids...)
						if end == n {
							assert.Empty(t, resp.NextCursor, "the last page carries no cursor")
							break
						}
						require.Equal(t, encodeRecommendationsCursor(want[end-1]), resp.NextCursor)
						params = map[string]string{"limit": strconv.Itoa(limit), "cursor": resp.NextCursor}
					}
					assert.Equal(t, want, got)
				})
			}
		}
	}
}

// TestGetRecommendations_PagedDuplicateIDs pins the tie handling to the
// in-memory paging: two surviving rows sharing an ID fail every paginated
// request, wherever the chunk and page boundaries fall and even when the
// duplicate lies beyond the requested page, while a duplicate whose twin a
// filter removes is not a tie at all.
func TestGetRecommendations_PagedDuplicateIDs(t *testing.T) {
	recs := scanFilterRecs()
	dup := recs[0] // "aws|b", in scope
	dup.Region = "us-west-1"
	recs = append(recs, dup)

	for _, chunk := range []int{1, 2, 3, len(recs)} {
		for _, limit := range []string{"1", "2", "100"} {
			h, _ := scanTestHandler(recs, chunk, false)
			_, err := h.getRecommendations(context.Background(), scopedRequest(""), map[string]string{"limit": limit})
			require.Error(t, err, "chunk=%d limit=%s", chunk, limit)
			assert.Contains(t, err.Error(), `duplicate recommendation ID "aws|b"`)
		}
	}

	// The twin is in an account outside the scoped reader's scope, so the
	// reader sees one "aws|b" and pages normally.
	outside := "22222222-2222-4222-8222-222222222222"
	recs[len(recs)-1].CloudAccountID = &outside
	h, _ := scanTestHandler(recs, 1, true)
	resp, err := h.getRecommendations(context.Background(), scopedRequest(""), map[string]string{"limit": "100"})
	require.NoError(t, err)
	assert.Len(t, resp.Recommendations, 8)
}

// TestGetRecommendations_PagedReadBackgroundCollectionScope keeps issue #627
// on the paginated path: only an unrestricted reader's scan may kick a
// background collection.
func TestGetRecommendations_PagedReadBackgroundCollectionScope(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		h, sched := scanTestHandler(scanFilterRecs(), 4, scoped)
		_, err := h.getRecommendations(context.Background(), scopedRequest(""), map[string]string{"limit": "3"})
		require.NoError(t, err)
		require.Len(t, sched.ctxs, 1, "one scan per request")
		assert.Equal(t, !scoped, scheduler.BackgroundCollectionAllowed(sched.ctxs[0]), "scoped=%v", scoped)
	}
}

// TestRecommendationsPageCollector_HoldsOnePage checks the memory bound: after
// a long scan the collector holds the page plus one look-ahead row, while
// the summary counts every row.
func TestRecommendationsPageCollector_HoldsOnePage(t *testing.T) {
	recs := pageTestRecs(2000)
	sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	c := newRecommendationsPageCollector(recommendationsPage{enabled: true, limit: 10, afterID: recs[99].ID})
	for start := 0; start < len(recs); start += 37 {
		require.NoError(t, c.add(recs[start:min(start+37, len(recs))]))
	}
	assert.Len(t, c.rows, 11, "page plus one look-ahead row")
	assert.Equal(t, recs[100].ID, c.rows[0].ID, "the page starts strictly after the cursor")

	resp, err := c.response(recommendationsPageByteBudget)
	require.NoError(t, err)
	assert.Len(t, resp.Recommendations, 10)
	assert.Equal(t, 2000, resp.Summary.TotalCount)
	assert.Equal(t, encodeRecommendationsCursor(recs[109].ID), resp.NextCursor)
}

// TestRecommendationsPageCollector_RejectsOutOfOrderScan fails loudly when the
// scan breaks its ordering contract, which would make the cursor skip rows.
func TestRecommendationsPageCollector_RejectsOutOfOrderScan(t *testing.T) {
	c := newRecommendationsPageCollector(recommendationsPage{enabled: true, limit: 10})
	require.NoError(t, c.add([]config.RecommendationRecord{{ID: "b"}}))
	err := c.add([]config.RecommendationRecord{{ID: "a"}})
	require.ErrorContains(t, err, "out of ID order")
}

// TestGetRecommendations_PagedEmptyCache returns an empty page as a JSON
// array, with zero totals and no cursor.
func TestGetRecommendations_PagedEmptyCache(t *testing.T) {
	h, _ := scanTestHandler(nil, 3, false)
	resp, err := h.getRecommendations(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"Authorization": "Bearer scoped-token"},
	}, map[string]string{"limit": "5"})
	require.NoError(t, err)
	require.NotNil(t, resp.Recommendations)
	assert.Empty(t, resp.Recommendations)
	assert.Empty(t, resp.NextCursor)
	assert.Equal(t, 0, resp.Summary.TotalCount)
}
