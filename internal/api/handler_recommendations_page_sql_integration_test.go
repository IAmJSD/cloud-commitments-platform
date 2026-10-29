//go:build integration

package api

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/scheduler"
)

// pageWalkStats records what a comparison walk covered, so the test can prove
// the fixture exercised the filters rather than comparing empty pages.
type pageWalkStats struct {
	rows, suppressed int
}

// comparePageWalk walks every page of one request through the SQL page read
// and through the Go-side scan (the pre-#981 path, kept as the oracle), page
// by page from the same cursor, and requires identical responses: rows,
// suppression badges, summary totals to the bit, regions and next_cursor.
func comparePageWalk(ctx context.Context, t *testing.T, f *pageSQLFixture, scope auth.AccountScope, params map[string]string, limit int) pageWalkStats {
	t.Helper()
	filter, err := parseRecommendationFilter(params)
	require.NoError(t, err)
	rows, err := f.h.recommendationsRowScope(ctx, scope, filter)
	require.NoError(t, err)
	var stats pageWalkStats
	afterID := ""
	for page := 0; ; page++ {
		require.Less(t, page, len(f.recs)+2, "cursor walk did not terminate")
		req := recommendationsPage{enabled: true, limit: limit, afterID: afterID}
		got, err := f.h.getRecommendationsPageFromSQL(ctx, scope, rows, req)
		require.NoError(t, err, "page %d: the SQL page read must not fall back on this fixture", page)
		want, err := f.h.getRecommendationsPageByScan(ctx, scope, filter, rows.keep, req)
		require.NoError(t, err)
		sort.Strings(got.Regions)
		sort.Strings(want.Regions)
		require.Equal(t, want, got, "page %d", page)
		for i := range got.Recommendations {
			rec := &got.Recommendations[i]
			if !scope.AllowsAll() {
				// An account-scoped caller must never see an out-of-scope row.
				require.NotNil(t, rec.CloudAccountID)
				require.Contains(t, rows.sql.AllowedAccountIDs, *rec.CloudAccountID)
			}
			if rec.SuppressedCount > 0 {
				stats.suppressed++
			}
		}
		stats.rows += len(got.Recommendations)
		if got.NextCursor == "" {
			assert.Equal(t, got.Summary.TotalCount, stats.rows, "the walk returns exactly the counted rows")
			return stats
		}
		afterID, err = decodeRecommendationsCursor(got.NextCursor)
		require.NoError(t, err)
	}
}

// TestRecommendationsPageSQL_MatchesScanOracle is the equivalence proof for
// issue #981: on a varied Postgres cache, every combination of enabled
// providers, account scope, query-string filter and page size returns, page
// by page, exactly what the Go-side scan returns. Account-scoped callers are
// included, and the summary they get must equal the scan's, which only
// counts in-scope rows.
func TestRecommendationsPageSQL_MatchesScanOracle(t *testing.T) {
	ctx := t.Context()
	f := newPageSQLFixture(ctx, t)
	var total pageWalkStats
	for _, enabled := range [][]string{nil, {"aws", "gcp"}} {
		require.NoError(t, f.store.SaveGlobalConfig(ctx, &config.GlobalConfig{EnabledProviders: enabled, DefaultTerm: 1, DefaultPayment: "all-upfront"}))
		for scopeName, scope := range f.scopes() {
			for filterName, params := range f.filters() {
				limits := []int{97, 1000}
				if filterName == "none" || filterName == "pct" {
					limits = append(limits, 7)
				}
				for _, limit := range limits {
					name := fmt.Sprintf("enabled=%v/scope=%s/filter=%s/limit=%d", enabled, scopeName, filterName, limit)
					t.Run(name, func(t *testing.T) {
						stats := comparePageWalk(ctx, t, f, scope, params, limit)
						total.rows += stats.rows
						total.suppressed += stats.suppressed
					})
				}
			}
		}
	}
	assert.Positive(t, total.rows)
	assert.Positive(t, total.suppressed, "the fixture must reach rows whose count a suppression reduced")
}

// TestRecommendationsPageSQL_FixtureFiltersBite checks that the fixture's
// filters drop rows, so the oracle comparison is not between two unfiltered
// lists.
func TestRecommendationsPageSQL_FixtureFiltersBite(t *testing.T) {
	ctx := t.Context()
	f := newPageSQLFixture(ctx, t)
	scope := auth.UnrestrictedScope()
	rows, err := f.h.recommendationsRowScope(ctx, scope, config.RecommendationFilter{})
	require.NoError(t, err)
	resp, err := f.h.getRecommendationsPageFromSQL(ctx, scope, rows, recommendationsPage{enabled: true, limit: 1})
	require.NoError(t, err)
	assert.Positive(t, resp.Summary.TotalCount)
	assert.Less(t, resp.Summary.TotalCount, len(f.recs), "suppressions and overrides must drop rows")
}

// pageFallbackHandler stores recs in a fresh database, runs edit (raw SQL
// for payload shapes the writer cannot produce) when set, and returns a
// handler reading them.
func pageFallbackHandler(ctx context.Context, t *testing.T, recs []config.RecommendationRecord, edit string) *Handler {
	t.Helper()
	pg := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")
	store := config.NewPostgresStore(pg.DB)
	require.NoError(t, store.UpsertRecommendations(ctx, time.Now().UTC(), recs, nil))
	if edit != "" {
		_, err := pg.DB.Pool().Exec(ctx, edit)
		require.NoError(t, err)
	}
	return &Handler{config: store, scheduler: scheduler.NewScheduler(scheduler.SchedulerConfig{ConfigStore: store, IsLambda: true})}
}

func fallbackRec(id, resourceType string, savings float64) config.RecommendationRecord {
	return config.RecommendationRecord{ID: id, Provider: "azure", Service: "compute", Region: "westeurope", ResourceType: resourceType,
		Count: 1, Term: 1, Payment: "no-upfront", Savings: savings}
}

// TestRecommendationsPageSQL_FallsBackToScan covers the inputs SQL cannot
// handle like Go. Each must make the SQL read report that it needs the scan,
// and the endpoint must then answer exactly as the scan does.
func TestRecommendationsPageSQL_FallsBackToScan(t *testing.T) {
	ctx := t.Context()
	cases := []struct {
		name    string
		recs    func() []config.RecommendationRecord
		params  map[string]string
		afterID string
		edit    string
	}{
		{"pct outside float-safe bounds", func() []config.RecommendationRecord {
			r := fallbackRec("a", "x1", 5)
			r.SavingsPercentage = pfloat(1e200)
			return []config.RecommendationRecord{r, fallbackRec("b", "x2", 6)}
		}, map[string]string{"min_savings_pct": "10"}, "", ""},
		{"savings outside float-safe bounds", func() []config.RecommendationRecord {
			return []config.RecommendationRecord{fallbackRec("a", "x1", 1e-200), fallbackRec("b", "x2", 6)}
		}, map[string]string{}, "", ""},
		{"cursor is not UTF-8", func() []config.RecommendationRecord {
			return []config.RecommendationRecord{fallbackRec("a", "x1", 5), fallbackRec("\xc3", "x2", 6), fallbackRec("z", "x3", 7)}
		}, map[string]string{}, "a\xff", ""},
		{"empty but present account", func() []config.RecommendationRecord {
			return []config.RecommendationRecord{fallbackRec("a", "x1", 5), fallbackRec("b", "x2", 6)}
		}, map[string]string{}, "", `UPDATE recommendations SET payload = jsonb_set(payload, '{cloud_account_id}', '""') WHERE payload->>'id' = 'a'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := pageFallbackHandler(ctx, t, tc.recs(), tc.edit)
			filter, err := parseRecommendationFilter(tc.params)
			require.NoError(t, err)
			scope := auth.UnrestrictedScope()
			rows, err := h.recommendationsRowScope(ctx, scope, filter)
			require.NoError(t, err)
			req := recommendationsPage{enabled: true, limit: 1, afterID: tc.afterID}
			_, err = h.getRecommendationsPageFromSQL(ctx, scope, rows, req)
			require.ErrorIs(t, err, scheduler.ErrRecommendationsPageNeedsScan)
			got, err := h.getRecommendationsPage(ctx, scope, filter, req)
			require.NoError(t, err)
			want, err := h.getRecommendationsPageByScan(ctx, scope, filter, rows.keep, req)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

// TestRecommendationsPageSQL_DuplicateIDsStillFail keeps the scan's refusal
// to page a cache where two rows share an ID: the SQL read hands the request
// to the scan, which fails it.
func TestRecommendationsPageSQL_DuplicateIDsStillFail(t *testing.T) {
	ctx := t.Context()
	h := pageFallbackHandler(ctx, t, []config.RecommendationRecord{fallbackRec("dup", "x1", 5), fallbackRec("dup", "x2", 6)}, "")
	scope := auth.UnrestrictedScope()
	rows, err := h.recommendationsRowScope(ctx, scope, config.RecommendationFilter{})
	require.NoError(t, err)
	_, err = h.getRecommendationsPageFromSQL(ctx, scope, rows, recommendationsPage{enabled: true, limit: 5})
	require.ErrorIs(t, err, scheduler.ErrRecommendationsPageNeedsScan)
	_, err = h.getRecommendationsPage(ctx, scope, config.RecommendationFilter{}, recommendationsPage{enabled: true, limit: 5})
	require.ErrorContains(t, err, "duplicate recommendation ID")
}
