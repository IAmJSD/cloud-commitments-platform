package api

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/scheduler"
)

// recommendationsScanChunkSize is how many cache rows the Go-side scan of a
// paginated GET /api/recommendations (the fallback of the SQL page read)
// reads per store query. Peak memory is about one chunk plus one page,
// whatever the tenant's size.
const recommendationsScanChunkSize = 500

// getRecommendationsPage serves a paginated GET /api/recommendations (issue
// #981). It asks the scheduler for the page with every list filter evaluated
// in SQL: the store totals the summary and regions in one aggregate query and
// returns only the page plus one row, so neither the rows read into Go nor
// the memory held grow with the tenant.
//
// When SQL cannot reproduce a Go filter faithfully for this request (see
// scheduler.PageRecommendations) the page is served by the Go-side scan
// instead, which reads every filtered row the way the endpoint did before.
func (h *Handler) getRecommendationsPage(ctx context.Context, scope auth.AccountScope, filter config.RecommendationFilter, page recommendationsPage) (*RecommendationsResponse, error) {
	rows, err := h.recommendationsRowScope(ctx, scope, filter)
	if err != nil {
		return nil, err
	}
	resp, err := h.getRecommendationsPageFromSQL(ctx, scope, rows, page)
	if !errors.Is(err, scheduler.ErrRecommendationsPageNeedsScan) {
		return resp, err
	}
	logging.Warnf("paginated recommendations: %v; serving the page from the scan", err)
	return h.getRecommendationsPageByScan(ctx, scope, filter, rows.keep, page)
}

// getRecommendationsPageFromSQL reads the page and the full-set summary from
// the scheduler's SQL page read. The rows are checked against the handler's
// own filters and the page order once more; a disagreement returns
// scheduler.ErrRecommendationsPageNeedsScan rather than a wrong page.
func (h *Handler) getRecommendationsPageFromSQL(ctx context.Context, scope auth.AccountScope, rows recommendationsRowScope, page recommendationsPage) (*RecommendationsResponse, error) {
	pg, err := h.scheduler.PageRecommendations(recommendationsReadContext(ctx, scope), rows.sql, page.afterID, page.limit+1)
	if err != nil {
		if errors.Is(err, scheduler.ErrRecommendationsPageNeedsScan) {
			return nil, err
		}
		return nil, fmt.Errorf("failed to get recommendations: %w", err)
	}
	if kept := rows.keep(append([]config.RecommendationRecord(nil), pg.Rows...)); len(kept) != len(pg.Rows) {
		return nil, fmt.Errorf("%w: SQL kept a row outside the enabled providers or account scope", scheduler.ErrRecommendationsPageNeedsScan)
	}
	collector := newRecommendationsPageCollector(page)
	if err := collector.setPage(pg); err != nil {
		return nil, fmt.Errorf("%w: %w", scheduler.ErrRecommendationsPageNeedsScan, err)
	}
	return collector.response(recommendationsPageByteBudget)
}

// getRecommendationsPageByScan is the Go-side page: it scans the cache in
// ID-ordered chunks through the scheduler (which applies suppressions and
// account overrides), drops rows the handler-side filters reject, and hands
// the survivors to a collector that keeps only the requested page while
// totaling every row for the summary. Each request reads the whole filtered
// set; only the memory held stays bounded.
func (h *Handler) getRecommendationsPageByScan(ctx context.Context, scope auth.AccountScope, filter config.RecommendationFilter, keep func([]config.RecommendationRecord) []config.RecommendationRecord, page recommendationsPage) (*RecommendationsResponse, error) {
	collector := newRecommendationsPageCollector(page)
	err := h.scheduler.ScanRecommendations(recommendationsReadContext(ctx, scope), filter, recommendationsScanChunkSize,
		func(chunk []config.RecommendationRecord) error {
			return collector.add(keep(chunk))
		})
	if err != nil {
		return nil, fmt.Errorf("failed to get recommendations: %w", err)
	}
	return collector.response(recommendationsPageByteBudget)
}

// recommendationsRowScope is the handler-side row filter of
// getRecommendations (enabled providers, issue #1409, then the caller's
// account scope) in two forms: keep applies it in Go, and sql carries it,
// with the request's filter, to the SQL page read.
type recommendationsRowScope struct {
	keep func([]config.RecommendationRecord) []config.RecommendationRecord
	sql  config.RecommendationListScope
}

// recommendationsRowScope reads the filters' inputs once up front rather
// than once per chunk.
func (h *Handler) recommendationsRowScope(ctx context.Context, scope auth.AccountScope, filter config.RecommendationFilter) (recommendationsRowScope, error) {
	var enabledProviders []string
	if h.config != nil {
		globalCfg, err := h.config.GetGlobalConfig(ctx)
		if err != nil {
			return recommendationsRowScope{}, fmt.Errorf("failed to read global config for enabled-providers filter: %w", err)
		}
		enabledProviders = globalCfg.EnabledProviders
	}
	out := recommendationsRowScope{sql: config.RecommendationListScope{Filter: filter, EnabledProviders: enabledProviders}}
	var nameByID map[string]string
	if !scope.AllowsAll() {
		var err error
		if nameByID, err = h.cloudAccountNamesByID(ctx); err != nil {
			return recommendationsRowScope{}, err
		}
		out.sql.Restricted = true
		out.sql.AllowedAccountIDs = scopeAllowedAccountIDs(scope, nameByID)
	}
	out.keep = func(recs []config.RecommendationRecord) []config.RecommendationRecord {
		recs = config.FilterRecommendationsByEnabledProviders(recs, enabledProviders)
		if scope.AllowsAll() {
			return recs
		}
		return filterRecommendationsInScope(scope, nameByID, recs)
	}
	return out, nil
}

// scopeAllowedAccountIDs is the exact set of account IDs
// filterRecommendationsInScope keeps for a restricted scope. It keeps a rec
// whose account is id when scope.Allows(id, nameByID[id]), which can only
// hold when id is on the allow-list or carries a registered name, so testing
// those candidates finds every allowed ID.
func scopeAllowedAccountIDs(scope auth.AccountScope, nameByID map[string]string) []string {
	ids := []string{}
	seen := make(map[string]struct{}, len(scope.Accounts)+len(nameByID))
	consider := func(id string) {
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		if scope.Allows(id, nameByID[id]) {
			ids = append(ids, id)
		}
	}
	for _, id := range scope.Accounts {
		consider(id)
	}
	for id := range nameByID {
		consider(id)
	}
	sort.Strings(ids)
	return ids
}

// recommendationsPageCollector builds one page from a scan's surviving rows,
// which arrive in strictly increasing ID order. It totals every row into the
// summary but keeps at most limit+1 rows after the cursor: the page, plus one
// row that proves a next page exists. The SQL page read fills it with
// setPage instead.
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

// setPage loads a page the store already filtered and totaled. The rows must
// be the first limit+1 after the cursor in strictly increasing ID order; any
// other order breaks the cursor walk and is an error.
func (c *recommendationsPageCollector) setPage(pg *config.RecommendationPage) error {
	prev := c.page.afterID
	for i := range pg.Rows {
		if id := pg.Rows[i].ID; (i > 0 || prev != "") && id <= prev {
			return fmt.Errorf("recommendations page out of ID order: %q after %q", id, prev)
		}
		prev = pg.Rows[i].ID
	}
	if len(pg.Rows) > c.page.limit+1 {
		return fmt.Errorf("recommendations page returned %d rows for limit %d", len(pg.Rows), c.page.limit)
	}
	c.rows = pg.Rows
	c.summary = recommendationsSummary{count: pg.TotalCount, totalSavings: pg.TotalSavings, totalUpfront: pg.TotalUpfront}
	for _, r := range pg.Regions {
		if c.summary.regionSet == nil {
			c.summary.regionSet = make(map[string]struct{}, len(pg.Regions))
		}
		c.summary.regionSet[r] = struct{}{}
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
