package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scanStore serves ListStoredRecommendationsChunk from recs the way the
// Postgres store does: ordered by ID (byte-wise) then by row position,
// resuming strictly after the given key. It also counts reads so tests can
// check that override lookups are not repeated per chunk.
type scanStore struct {
	mockOverrideStore
	sups           []config.PurchaseSuppression
	supsErr        error
	chunkErr       error
	chunkReads     int
	overrideLookup map[string]int
}

func (m *scanStore) ListActiveSuppressions(_ context.Context) ([]config.PurchaseSuppression, error) {
	return m.sups, m.supsErr
}

func (m *scanStore) GetAccountServiceOverride(ctx context.Context, accountID, provider, service string) (*config.AccountServiceOverride, error) {
	if m.overrideLookup == nil {
		m.overrideLookup = map[string]int{}
	}
	m.overrideLookup[accountID+"|"+provider+"|"+service]++
	return m.mockOverrideStore.GetAccountServiceOverride(ctx, accountID, provider, service)
}

func (m *scanStore) ListStoredRecommendationsChunk(_ context.Context, _ config.RecommendationFilter, after *config.RecommendationScanKey, limit int) (config.RecommendationChunk, error) {
	m.chunkReads++
	if m.chunkErr != nil {
		return config.RecommendationChunk{}, m.chunkErr
	}
	type row struct {
		key config.RecommendationScanKey
		rec config.RecommendationRecord
	}
	rows := make([]row, len(m.recs))
	for i := range m.recs {
		rows[i] = row{config.RecommendationScanKey{ID: m.recs[i].ID, RowID: fmt.Sprintf("%08d", i)}, m.recs[i]}
	}
	less := func(a, b config.RecommendationScanKey) bool {
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.RowID < b.RowID
	}
	sort.Slice(rows, func(i, j int) bool { return less(rows[i].key, rows[j].key) })
	var chunk config.RecommendationChunk
	for i := range rows {
		if after != nil && !less(*after, rows[i].key) {
			continue
		}
		if len(chunk.Recs) == limit {
			return chunk, nil
		}
		chunk.Recs = append(chunk.Recs, rows[i].rec)
		k := rows[i].key
		chunk.Last = &k
	}
	// Like the store: only a short read proves there is nothing after Last.
	chunk.Done = len(chunk.Recs) < limit
	return chunk, nil
}

// scanFixture is a cache where suppressions and account overrides each drop
// some rows and change others, spread over several chunks.
func scanFixture() *scanStore {
	var recs []config.RecommendationRecord
	for i := 9; i >= 0; i-- { // reverse order: the scan must not rely on input order
		for _, acct := range []string{"acct-A", "acct-B", "acct-C"} {
			rec := rdsRec(acct, "us-east-1", "mysql")
			rec.ID = fmt.Sprintf("%s|%02d", acct, i)
			rec.ResourceType = fmt.Sprintf("db.t3.r%02d", i)
			rec.Count = 2
			recs = append(recs, rec)
		}
	}
	future := time.Now().Add(time.Hour)
	return &scanStore{
		mockOverrideStore: mockOverrideStore{
			recs: recs,
			globals: map[string]*config.ServiceConfig{
				"aws|rds": {Provider: "aws", Service: "rds", Enabled: true},
			},
			overrides: map[string]*config.AccountServiceOverride{
				"acct-B|aws|rds": {Enabled: boolPtr(false)},
			},
		},
		sups: []config.PurchaseSuppression{
			// Drops acct-C|03 (2 - 2 = 0) and annotates acct-A|05 (2 - 1 = 1).
			{ExecutionID: "e1", AccountID: "acct-C", Provider: "aws", Service: "rds", Region: "us-east-1",
				ResourceType: "db.t3.r03", Engine: "mysql", SuppressedCount: 2, ExpiresAt: future},
			{ExecutionID: "e2", AccountID: "acct-A", Provider: "aws", Service: "rds", Region: "us-east-1",
				ResourceType: "db.t3.r05", Engine: "mysql", SuppressedCount: 1, ExpiresAt: future},
		},
	}
}

func collectScan(t *testing.T, s *Scheduler, chunkSize int) ([]config.RecommendationRecord, int) {
	t.Helper()
	var got []config.RecommendationRecord
	calls := 0
	err := s.ScanRecommendations(context.Background(), config.RecommendationFilter{}, chunkSize, func(chunk []config.RecommendationRecord) error {
		calls++
		require.NotEmpty(t, chunk, "fn must not be called with an empty chunk")
		got = append(got, chunk...)
		return nil
	})
	require.NoError(t, err)
	return got, calls
}

// TestScanRecommendations_MatchesListRecommendations pins the scan to the
// legacy read: for every chunk size it yields exactly the rows
// ListRecommendations returns, with the same suppression counts and override
// drops, in ID order.
func TestScanRecommendations_MatchesListRecommendations(t *testing.T) {
	store := scanFixture()
	s := &Scheduler{config: store}

	want, err := s.ListRecommendations(context.Background(), config.RecommendationFilter{})
	require.NoError(t, err)
	sort.Slice(want, func(i, j int) bool { return want[i].ID < want[j].ID })
	require.Len(t, want, 19, "fixture: 30 rows, acct-B's 10 overridden away, one suppressed away")

	for _, chunkSize := range []int{1, 2, 3, 7, 30, 31} {
		t.Run(fmt.Sprintf("chunk=%d", chunkSize), func(t *testing.T) {
			store.overrideLookup = nil
			got, _ := collectScan(t, s, chunkSize)
			assert.Equal(t, want, got)
			for key, n := range store.overrideLookup {
				assert.Equal(t, 1, n, "override for %s looked up %d times in one scan", key, n)
			}
		})
	}
}

// TestScanRecommendations_SkipsEmptyChunks checks that a chunk whose rows are
// all filtered away is not passed to fn, and that the walk still continues
// past it.
func TestScanRecommendations_SkipsEmptyChunks(t *testing.T) {
	store := scanFixture()
	s := &Scheduler{config: store}
	got, calls := collectScan(t, s, 1)
	assert.Len(t, got, 19)
	assert.Equal(t, 19, calls, "one call per surviving row with chunk size 1")
	assert.Equal(t, 31, store.chunkReads, "30 one-row chunks and the empty final read")
}

// TestScanRecommendations_StopsOnCallbackError propagates fn's error and
// reads no further chunks.
func TestScanRecommendations_StopsOnCallbackError(t *testing.T) {
	store := scanFixture()
	s := &Scheduler{config: store}
	boom := errors.New("boom")
	err := s.ScanRecommendations(context.Background(), config.RecommendationFilter{}, 5, func([]config.RecommendationRecord) error {
		return boom
	})
	require.ErrorIs(t, err, boom)
	assert.Equal(t, 1, store.chunkReads)
}

// TestScanRecommendations_StoreErrorFails returns a chunk read error instead
// of a short walk.
func TestScanRecommendations_StoreErrorFails(t *testing.T) {
	store := scanFixture()
	store.chunkErr = errors.New("db down")
	s := &Scheduler{config: store}
	err := s.ScanRecommendations(context.Background(), config.RecommendationFilter{}, 5, func([]config.RecommendationRecord) error {
		t.Fatal("fn must not run when the read fails")
		return nil
	})
	require.ErrorContains(t, err, "db down")
}

// TestScanRecommendations_ReadFailuresOverShow mirrors ListRecommendations'
// trade-off: a suppression or override read failure is logged and the rows
// pass through rather than failing the page.
func TestScanRecommendations_ReadFailuresOverShow(t *testing.T) {
	store := scanFixture()
	store.supsErr = errors.New("sups down")
	store.getOverrideErr = errors.New("overrides down")
	s := &Scheduler{config: store}
	got, _ := collectScan(t, s, 4)
	assert.Len(t, got, 30, "nothing suppressed or overridden away")
}

// TestScanRecommendations_RejectsBadChunkSize guards the loop bound.
func TestScanRecommendations_RejectsBadChunkSize(t *testing.T) {
	s := &Scheduler{config: scanFixture()}
	err := s.ScanRecommendations(context.Background(), config.RecommendationFilter{}, 0, func([]config.RecommendationRecord) error { return nil })
	require.Error(t, err)
}
