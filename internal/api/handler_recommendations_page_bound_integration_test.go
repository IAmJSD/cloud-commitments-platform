//go:build integration

package api

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/scheduler"
)

// chunkCountingStore counts the rows the Go-side scan pulls out of Postgres.
type chunkCountingStore struct {
	*config.PostgresStore
	chunkRows atomic.Int64
}

func (s *chunkCountingStore) ListStoredRecommendationsChunk(ctx context.Context, filter config.RecommendationFilter, after *config.RecommendationScanKey, limit int) (config.RecommendationChunk, error) {
	chunk, err := s.PostgresStore.ListStoredRecommendationsChunk(ctx, filter, after, limit)
	s.chunkRows.Add(int64(len(chunk.Recs)))
	return chunk, err
}

// TestGetRecommendations_PageDoesNotReadEveryRow is the issue #981
// regression: a paginated GET /api/recommendations must not read the whole
// filtered cache into the process to total the summary. Before the fix every
// page request pulled every row through ListStoredRecommendationsChunk; now
// the summary is an SQL aggregate and only the page reaches Go. The walk
// must still return the legacy rows and totals.
func TestGetRecommendations_PageDoesNotReadEveryRow(t *testing.T) {
	ctx := t.Context()
	f := newPageSQLFixture(ctx, t)
	store := &chunkCountingStore{PostgresStore: f.store}
	sched := scheduler.NewScheduler(scheduler.SchedulerConfig{ConfigStore: store, IsLambda: true})
	h := &Handler{config: store, scheduler: sched, apiKey: "test-key"}

	legacy, err := h.getRecommendations(ctx, pageTestReq(), map[string]string{"min_savings_pct": "20"})
	require.NoError(t, err)
	require.Greater(t, len(legacy.Recommendations), 100)

	const limit = 50
	params := map[string]string{"min_savings_pct": "20", "limit": strconv.Itoa(limit)}
	walked := 0
	for page := 0; ; page++ {
		require.Less(t, page, len(legacy.Recommendations), "cursor walk did not terminate")
		resp, err := h.getRecommendations(ctx, pageTestReq(), params)
		require.NoError(t, err)
		assert.Equal(t, legacy.Summary.TotalCount, resp.Summary.TotalCount)
		// The unpaginated total sums in table order, the paginated one in ID
		// order, so the float totals may differ in the last bits.
		assert.InDelta(t, legacy.Summary.TotalMonthlySavings, resp.Summary.TotalMonthlySavings, 1e-6*legacy.Summary.TotalMonthlySavings)
		assert.ElementsMatch(t, legacy.Regions, resp.Regions)
		walked += len(resp.Recommendations)
		if resp.NextCursor == "" {
			break
		}
		params["cursor"] = resp.NextCursor
	}
	assert.Equal(t, len(legacy.Recommendations), walked)
	assert.Zero(t, store.chunkRows.Load(), "a paginated request must not scan the cache into Go")
}

// TestGetRecommendations_PageAndSummaryShareOneSnapshot races paginated reads
// against collections that rewrite every row in place. Each collection
// commits atomically, so every response must describe one collection: a
// summary from one and rows from another, or a summary mixing both, would
// show totals the cache never held. The SQL page read takes its summary and
// its rows from one REPEATABLE READ snapshot; a read made of separately
// committed statements (as the chunked scan was) can mix them.
func TestGetRecommendations_PageAndSummaryShareOneSnapshot(t *testing.T) {
	ctx := t.Context()
	pg := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")
	store := config.NewPostgresStore(pg.DB)
	const n = 1200
	collection := func(savings float64) []config.RecommendationRecord {
		recs := make([]config.RecommendationRecord, n)
		for i := range recs {
			recs[i] = config.RecommendationRecord{ID: fmt.Sprintf("rec|%05d", i), Provider: "aws", Service: "rds", Region: "us-east-1",
				ResourceType: fmt.Sprintf("db.r%05d", i), Count: 1, Term: 1, Payment: "no-upfront", Savings: savings}
		}
		return recs
	}
	require.NoError(t, store.UpsertRecommendations(ctx, time.Now(), collection(1), nil))
	h := &Handler{config: store, scheduler: scheduler.NewScheduler(scheduler.SchedulerConfig{ConfigStore: store, IsLambda: true}), apiKey: "test-key"}

	raceCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var writes atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; raceCtx.Err() == nil; i++ {
			if err := store.UpsertRecommendations(ctx, time.Now(), collection(float64(1+i%2)), nil); err != nil {
				t.Errorf("upsert: %v", err)
				return
			}
			writes.Add(1)
		}
	}()
	var reads atomic.Int64
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			cursor := encodeRecommendationsCursor(fmt.Sprintf("rec|%05d", (r*271)%n))
			for raceCtx.Err() == nil {
				resp, err := h.getRecommendations(ctx, pageTestReq(), map[string]string{"limit": "100", "cursor": cursor})
				if !assert.NoError(t, err) {
					return
				}
				total := resp.Summary.TotalMonthlySavings
				if !assert.Contains(t, []float64{n, 2 * n}, total, "summary mixes two collections") {
					return
				}
				for i := range resp.Recommendations {
					if !assert.Equal(t, total/n, resp.Recommendations[i].Savings, "page row from another collection than its summary") {
						return
					}
				}
				reads.Add(1)
			}
		}(r)
	}
	wg.Wait()
	t.Logf("%d page reads raced %d collections", reads.Load(), writes.Load())
	require.Positive(t, writes.Load())
	require.Positive(t, reads.Load())
}
