//go:build integration

package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
)

// Issue #25: the Savings History queries must honor the global Provider
// filter. The fixture puts one purchase_history row per provider in the
// window, each with its own service and savings so the result names exactly
// which rows were aggregated, plus one row of each provider outside the window
// to show the date bounds still apply alongside the provider predicate.
//
// The package has no TestMain that stops the shared container; Ryuk reaps it
// when the test binary exits (see testhelpers.TerminateSharedPostgres).

type providerFilterRow struct {
	provider, service string
	savings           float64
}

var providerFilterRows = []providerFilterRow{
	{"aws", "svc-aws", 1},
	{"azure", "svc-azure", 10},
	{"gcp", "svc-gcp", 100},
}

func seedProviderFilterRows(ctx context.Context, t *testing.T, pg *testhelpers.PostgresContainer, inWindow, outOfWindow time.Time) {
	t.Helper()
	for i, r := range providerFilterRows {
		for _, at := range []struct {
			suffix string
			ts     time.Time
		}{{"in", inWindow}, {"out", outOfWindow}} {
			_, err := pg.DB.Pool().Exec(ctx, `
				INSERT INTO purchase_history (account_id, purchase_id, timestamp, provider, service, region,
					resource_type, count, term, payment, upfront_cost, monthly_cost, estimated_savings)
				VALUES ($1, $2, $3, $4, $5, 'us-east-1', 'm5.large', 1, 1, 'all-upfront', 5, 0, $6)`,
				fmt.Sprintf("acct-%d", i), fmt.Sprintf("pf-%s-%s", r.provider, at.suffix), at.ts, r.provider, r.service, r.savings)
			require.NoError(t, err)
		}
	}
}

func TestAnalyticsProviderFilter_DB(t *testing.T) {
	ctx := t.Context()
	pg := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")

	now := time.Now().UTC().Truncate(time.Microsecond)
	start, end := now.Add(-24*time.Hour), now.Add(time.Hour)
	seedProviderFilterRows(ctx, t, pg, now.Add(-time.Hour), now.Add(-72*time.Hour))
	client := NewPostgresAnalyticsClient(pg.DB)

	allServices := map[string]float64{"svc-aws": 1, "svc-azure": 10, "svc-gcp": 100}
	cases := []struct {
		name     string
		provider string
		want     map[string]float64 // service -> savings
	}{
		{"aws only", "aws", map[string]float64{"svc-aws": 1}},
		{"azure only", "azure", map[string]float64{"svc-azure": 10}},
		{"gcp only", "gcp", map[string]float64{"svc-gcp": 100}},
		{"no provider returns every provider", "", allServices},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var wantTotal float64
			for _, v := range tc.want {
				wantTotal += v
			}

			points, summary, err := client.QueryHistory(ctx, nil, nil, tc.provider, start, end, "daily")
			require.NoError(t, err)
			gotHistory := map[string]float64{}
			for _, p := range points {
				for svc, v := range p.ByService {
					gotHistory[svc] += v
				}
			}
			assert.Equal(t, tc.want, gotHistory, "QueryHistory by_service")
			require.NotNil(t, summary)
			assert.Equal(t, len(tc.want), summary.TotalPurchases)
			assert.InDelta(t, wantTotal, summary.TotalMonthlySavings, 1e-9)

			bySvc, err := client.QueryBreakdown(ctx, nil, nil, tc.provider, start, end, "service")
			require.NoError(t, err)
			gotBreakdown := map[string]float64{}
			var pct float64
			for svc, v := range bySvc {
				gotBreakdown[svc] = v.TotalSavings
				pct += v.Percentage
			}
			assert.Equal(t, tc.want, gotBreakdown, "QueryBreakdown by service")
			assert.InDelta(t, 100.0, pct, 1e-9, "percentages are of the filtered total")

			byProvider, err := client.QueryBreakdown(ctx, nil, nil, tc.provider, start, end, "provider")
			require.NoError(t, err)
			if tc.provider != "" {
				require.Len(t, byProvider, 1)
				assert.Contains(t, byProvider, tc.provider)
			} else {
				assert.Len(t, byProvider, len(providerFilterRows))
			}
		})
	}
}

// TestAnalyticsProviderFilter_HandlerAllIsNoFilter_DB drives both handlers
// against the database: provider=all and an absent provider return every
// provider's rows, and provider=gcp returns only gcp's.
func TestAnalyticsProviderFilter_HandlerAllIsNoFilter_DB(t *testing.T) {
	ctx := t.Context()
	pg := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")

	now := time.Now().UTC().Truncate(time.Microsecond)
	seedProviderFilterRows(ctx, t, pg, now.Add(-time.Hour), now.Add(-72*time.Hour))
	window := map[string]string{
		"start": now.Add(-24 * time.Hour).Format(time.RFC3339),
		"end":   now.Add(time.Hour).Format(time.RFC3339),
	}

	for _, tc := range []struct {
		name   string
		params map[string]string
		want   int
	}{
		{"absent", map[string]string{}, 3},
		{"all", map[string]string{"provider": "all"}, 3},
		{"gcp", map[string]string{"provider": "gcp"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mockAuth, req := adminAnalyticsReq(ctx)
			h := &Handler{auth: mockAuth, analyticsClient: NewPostgresAnalyticsClient(pg.DB), config: new(MockConfigStore)}
			params := map[string]string{"interval": "daily"}
			for k, v := range window {
				params[k] = v
			}
			for k, v := range tc.params {
				params[k] = v
			}

			got, err := h.getHistoryAnalytics(ctx, req, params)
			require.NoError(t, err)
			resp, ok := got.(*AnalyticsResponse)
			require.True(t, ok)
			assert.Equal(t, tc.want, resp.Summary.TotalPurchases, "history analytics")

			params["dimension"] = "provider"
			got, err = h.getHistoryBreakdown(ctx, req, params)
			require.NoError(t, err)
			breakdown, ok := got.(*BreakdownResponse)
			require.True(t, ok)
			assert.Len(t, breakdown.Data, tc.want, "history breakdown by provider")
			if p := tc.params["provider"]; p != "" && p != "all" {
				assert.Contains(t, breakdown.Data, p)
			}
		})
	}
}
