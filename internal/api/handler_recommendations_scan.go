package api

import (
	"context"
	"fmt"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// recommendationsScanChunkSize is how many cache rows a paginated
// GET /api/recommendations reads per store query. Peak memory is about one
// chunk plus one page, whatever the tenant's size.
const recommendationsScanChunkSize = 500

// getRecommendationsPage serves a paginated GET /api/recommendations (issue
// #981). Instead of loading and sorting the whole filtered set, it scans the
// cache in ID-ordered chunks through the scheduler (which applies
// suppressions and account overrides), drops rows for disabled providers and
// accounts outside the caller's scope, and hands the survivors to a collector
// that keeps only the requested page while totaling every row for the
// summary.
//
// The summary still needs every filtered row, so each request still reads the
// whole filtered set from the store; what no longer grows with the tenant is
// the memory held and the in-process sort. Pushing the summary into SQL would
// need suppressions, account overrides, the provider and scope filters and
// the savings-percentage floor in SQL too; until then a SQL aggregate would
// count rows the response hides.
func (h *Handler) getRecommendationsPage(ctx context.Context, scope auth.AccountScope, filter config.RecommendationFilter, page recommendationsPage) (*RecommendationsResponse, error) {
	keep, err := h.recommendationsRowFilter(ctx, scope)
	if err != nil {
		return nil, err
	}
	collector := newRecommendationsPageCollector(page)
	err = h.scheduler.ScanRecommendations(recommendationsReadContext(ctx, scope), filter, recommendationsScanChunkSize,
		func(chunk []config.RecommendationRecord) error {
			return collector.add(keep(chunk))
		})
	if err != nil {
		return nil, fmt.Errorf("failed to get recommendations: %w", err)
	}
	return collector.response(recommendationsPageByteBudget)
}

// recommendationsRowFilter returns the handler-side row filters of
// getRecommendations (enabled providers, issue #1409, then the caller's
// account scope) as one function, with their inputs read once up front
// rather than once per chunk.
func (h *Handler) recommendationsRowFilter(ctx context.Context, scope auth.AccountScope) (func([]config.RecommendationRecord) []config.RecommendationRecord, error) {
	var enabledProviders []string
	if h.config != nil {
		globalCfg, err := h.config.GetGlobalConfig(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to read global config for enabled-providers filter: %w", err)
		}
		enabledProviders = globalCfg.EnabledProviders
	}
	var nameByID map[string]string
	if !scope.AllowsAll() {
		var err error
		if nameByID, err = h.cloudAccountNamesByID(ctx); err != nil {
			return nil, err
		}
	}
	return func(recs []config.RecommendationRecord) []config.RecommendationRecord {
		recs = config.FilterRecommendationsByEnabledProviders(recs, enabledProviders)
		if scope.AllowsAll() {
			return recs
		}
		return filterRecommendationsInScope(scope, nameByID, recs)
	}, nil
}

// recommendationsPageCollector builds one page from a scan's surviving rows,
// which arrive in strictly increasing ID order. It totals every row into the
// summary but keeps at most limit+1 rows after the cursor: the page, plus one
// row that proves a next page exists.
type recommendationsPageCollector struct {
	page    recommendationsPage
	summary recommendationsSummary
	seenAny bool
	lastID  string
	rows    []config.RecommendationRecord
}

func newRecommendationsPageCollector(page recommendationsPage) *recommendationsPageCollector {
	return &recommendationsPageCollector{page: page, rows: []config.RecommendationRecord{}}
}

// add consumes the next rows of the scan. Paging needs ID order to be a total
// order: two rows sharing an ID would let the strict "after" comparison skip
// the second one at a page boundary, so a duplicate fails the request rather
// than silently walking short. The ID is the application recommendation ID
// built from the natural-key columns (recommendationID in
// internal/scheduler), not the cache table's UUID primary key. A row out of
// order means the scan broke its contract and would make the cursor skip
// rows, so it fails too.
func (c *recommendationsPageCollector) add(recs []config.RecommendationRecord) error {
	for i := range recs {
		rec := &recs[i]
		if c.seenAny && rec.ID <= c.lastID {
			if rec.ID == c.lastID {
				return fmt.Errorf("duplicate recommendation ID %q: cannot page the recommendations list", rec.ID)
			}
			return fmt.Errorf("recommendations scan out of ID order: %q after %q", rec.ID, c.lastID)
		}
		c.seenAny, c.lastID = true, rec.ID
		c.summary.add(rec)
		// Resuming after the last ID the client saw (rather than at an
		// offset) means a row added or removed between two requests does not
		// shift later pages, even if the cursor row itself is gone.
		if len(c.rows) <= c.page.limit && (c.page.afterID == "" || rec.ID > c.page.afterID) {
			c.rows = append(c.rows, *rec)
		}
	}
	return nil
}

// response cuts the collected rows to one page by row count and serialized
// bytes (see selectRecommendationsPage) and sets next_cursor when any row is
// left after it.
func (c *recommendationsPageCollector) response(budget int) (*RecommendationsResponse, error) {
	resp := c.summary.response(c.rows[:0])
	// Overhead: everything except the rows and the cursor value, measured
	// with an empty next_cursor key present.
	resp.NextCursor = " "
	overhead, err := envelopeLen(resp)
	if err != nil {
		return nil, fmt.Errorf("failed to size recommendations page: %w", err)
	}
	n, err := selectRecommendationsPage(c.rows, 0, c.page.limit, overhead, budget)
	if err != nil {
		return nil, err
	}
	resp.Recommendations = c.rows[:n]
	resp.NextCursor = ""
	if n < len(c.rows) {
		resp.NextCursor = encodeRecommendationsCursor(c.rows[n-1].ID)
	}
	return resp, nil
}
