//go:build integration

package api

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/scheduler"
)

// TestGetRecommendations_PagedScanMatchesLegacyOnPostgres runs the paginated
// GET /api/recommendations end to end on a real migrated database (issue
// #981): handler, scheduler scan and the keyset chunk query. The cache holds
// more rows than one scan chunk, with IDs whose byte order differs from a
// linguistic order, and the request carries the in-process
// min_savings_pct floor. Walking the cursor must return exactly the legacy
// unpaginated rows in Go string order, with the legacy summary on every page.
func TestGetRecommendations_PagedScanMatchesLegacyOnPostgres(t *testing.T) {
	ctx := t.Context()
	pg, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Cleanup(context.Background())) })
	require.NoError(t, migrations.RunMigrations(ctx, pg.DB.Pool(), "../database/postgres/migrations", "", ""))

	store := config.NewPostgresStore(pg.DB)
	prefixes := []string{"aws", "AWS", "aws-x", "aws x", "Aws", "élan", "elan", "_z", "aws/x"}
	const total = 2*recommendationsScanChunkSize + 37
	recs := make([]config.RecommendationRecord, total)
	for i := range recs {
		pct := float64(10 + i%50) // rows with pct < 20 fall under the floor below
		recs[i] = config.RecommendationRecord{
			ID:                fmt.Sprintf("%s|%04d", prefixes[i%len(prefixes)], total-i),
			Provider:          "aws",
			Service:           "ec2",
			Region:            "us-east-" + strconv.Itoa(i%3+1),
			ResourceType:      fmt.Sprintf("m5.r%04d", i),
			Count:             1,
			Term:              1,
			Payment:           "no-upfront",
			Savings:           float64(i % 7),
			UpfrontCost:       float64(i % 5),
			SavingsPercentage: &pct,
		}
	}
	require.NoError(t, store.UpsertRecommendations(ctx, time.Now().UTC(), recs, nil))

	sched := scheduler.NewScheduler(scheduler.SchedulerConfig{ConfigStore: store, IsLambda: true})
	h := &Handler{config: store, scheduler: sched, apiKey: "test-key"}

	legacy, err := h.getRecommendations(ctx, pageTestReq(), map[string]string{"min_savings_pct": "20"})
	require.NoError(t, err)
	want := make([]string, len(legacy.Recommendations))
	for i := range legacy.Recommendations {
		want[i] = legacy.Recommendations[i].ID
	}
	sort.Strings(want)
	require.Greater(t, len(want), recommendationsScanChunkSize, "the walk must span several scan chunks")
	require.Less(t, len(want), total, "the pct floor must drop some rows")

	const limit = 97
	var got []string
	params := map[string]string{"min_savings_pct": "20", "limit": strconv.Itoa(limit)}
	for page := 0; ; page++ {
		require.LessOrEqual(t, page, len(want)/limit+1, "cursor walk did not terminate")
		resp, err := h.getRecommendations(ctx, pageTestReq(), params)
		require.NoError(t, err)
		assert.Equal(t, legacy.Summary, resp.Summary, "page %d summary", page)
		assert.ElementsMatch(t, legacy.Regions, resp.Regions)
		for i := range resp.Recommendations {
			got = append(got, resp.Recommendations[i].ID)
		}
		if resp.NextCursor == "" {
			break
		}
		require.Len(t, resp.Recommendations, limit, "only the last page may be short")
		params = map[string]string{"min_savings_pct": "20", "limit": strconv.Itoa(limit), "cursor": resp.NextCursor}
	}
	assert.Equal(t, want, got)
}
