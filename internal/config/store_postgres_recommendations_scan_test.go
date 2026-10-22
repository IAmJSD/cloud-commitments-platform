//go:build integration
// +build integration

package config_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scanRec is an AWS rec with the given application ID. resourceType must be
// unique per row: it is the part of the natural key these tests vary.
func scanRec(id, resourceType string) config.RecommendationRecord {
	return awsRec(id, "ec2", "us-east-1", resourceType, 10)
}

// walkChunks reads every chunk for filter, chunkSize rows at a time, and
// returns the IDs of the recs in scan order plus the number of reads.
func walkChunks(ctx context.Context, t *testing.T, store *config.PostgresStore, filter config.RecommendationFilter, chunkSize int) (ids []string, reads int) {
	t.Helper()
	var after *config.RecommendationScanKey
	for {
		reads++
		require.Less(t, reads, 10000, "chunk walk did not terminate")
		chunk, err := store.ListStoredRecommendationsChunk(ctx, filter, after, chunkSize)
		require.NoError(t, err)
		for i := range chunk.Recs {
			ids = append(ids, chunk.Recs[i].ID)
		}
		if chunk.Done {
			return ids, reads
		}
		require.NotNil(t, chunk.Last, "a chunk that is not done must say where to resume")
		after = chunk.Last
	}
}

// TestListStoredRecommendationsChunk_OrderMatchesGoStringOrder pins the scan
// order to Go's byte-wise string order (issue #981), which is the order the
// paginated API sorted rows into before the keyset moved into SQL. The IDs mix
// case, "|", "-", "/", spaces, non-ASCII and the empty ID, where a linguistic
// collation (en_US and friends) orders differently from bytes.
func TestListStoredRecommendationsChunk_OrderMatchesGoStringOrder(t *testing.T) {
	ctx := context.Background()
	store := setupRecommendationsStore(ctx, t)

	ids := []string{
		"aws|b", "AWS|a", "aws-a", "aws a", "aws/a", "aws|a|Linux/UNIX|default",
		"aws|a", "Zeta", "alpha", "élan", "elan", "_x", "", "aws|a|Windows|default",
	}
	recs := make([]config.RecommendationRecord, len(ids))
	for i, id := range ids {
		recs[i] = scanRec(id, fmt.Sprintf("t%02d", i))
	}
	require.NoError(t, store.UpsertRecommendations(ctx, time.Now().UTC(), recs, nil))

	want := append([]string(nil), ids...)
	sort.Strings(want)

	for _, chunkSize := range []int{1, 2, 3, len(ids), len(ids) + 1} {
		t.Run(fmt.Sprintf("chunk=%d", chunkSize), func(t *testing.T) {
			got, reads := walkChunks(ctx, t, store, config.RecommendationFilter{}, chunkSize)
			assert.Equal(t, want, got, "scan order must equal Go's sort.Strings order")
			// The walk ends on the first short chunk: exactly enough reads to
			// cover every row plus, when the count divides evenly, one empty
			// read to learn there is nothing after the last row.
			assert.Equal(t, len(ids)/chunkSize+1, reads)
		})
	}
}

// TestListStoredRecommendationsChunk_DuplicateIDsNeverSkipped pins the
// tie-break: rows that share an application ID are ordered by primary key, and
// a chunk boundary that falls inside such a run neither skips nor repeats a
// row, so a caller further up sees every duplicate and can reject it.
func TestListStoredRecommendationsChunk_DuplicateIDsNeverSkipped(t *testing.T) {
	ctx := context.Background()
	store := setupRecommendationsStore(ctx, t)

	recs := []config.RecommendationRecord{
		scanRec("a", "t1"), scanRec("b", "t2"), scanRec("b", "t3"), scanRec("b", "t4"), scanRec("c", "t5"),
	}
	require.NoError(t, store.UpsertRecommendations(ctx, time.Now().UTC(), recs, nil))

	for chunkSize := 1; chunkSize <= len(recs)+1; chunkSize++ {
		got, _ := walkChunks(ctx, t, store, config.RecommendationFilter{}, chunkSize)
		assert.Equal(t, []string{"a", "b", "b", "b", "c"}, got, "chunk size %d", chunkSize)
	}
}

// TestListStoredRecommendationsChunk_FiltersAndPctFloor checks the SQL filters
// still apply, and that rows dropped by the in-process MinSavingsPct floor
// still advance the position, so a chunk whose rows are all below the floor is
// empty but not the end of the walk.
func TestListStoredRecommendationsChunk_FiltersAndPctFloor(t *testing.T) {
	ctx := context.Background()
	store := setupRecommendationsStore(ctx, t)

	pct := func(id, resourceType string, p float64) config.RecommendationRecord {
		rec := scanRec(id, resourceType)
		rec.SavingsPercentage = &p
		return rec
	}
	other := scanRec("r00", "t0")
	other.Region = "eu-west-1"
	recs := []config.RecommendationRecord{
		other,
		pct("r01", "t1", 5), pct("r02", "t2", 5), // a whole chunk below the floor
		pct("r03", "t3", 50), pct("r04", "t4", 5), pct("r05", "t5", 60),
	}
	require.NoError(t, store.UpsertRecommendations(ctx, time.Now().UTC(), recs, nil))

	filter := config.RecommendationFilter{Region: "us-east-1", MinSavingsPct: 30}
	first, err := store.ListStoredRecommendationsChunk(ctx, filter, nil, 2)
	require.NoError(t, err)
	assert.Empty(t, first.Recs, "both rows of the first chunk are below the floor")
	assert.False(t, first.Done, "a full chunk is not the end even when the floor emptied it")
	require.NotNil(t, first.Last)
	assert.Equal(t, "r02", first.Last.ID)
	assert.NotEmpty(t, first.Last.RowID)

	got, _ := walkChunks(ctx, t, store, filter, 2)
	assert.Equal(t, []string{"r03", "r05"}, got)

	_, err = store.ListStoredRecommendationsChunk(ctx, filter, nil, 0)
	require.Error(t, err, "a non-positive limit is a caller bug, not an empty result")
}

// TestListStoredRecommendationsChunk_EmptyTable returns a done, empty chunk
// with no position.
func TestListStoredRecommendationsChunk_EmptyTable(t *testing.T) {
	ctx := context.Background()
	store := setupRecommendationsStore(ctx, t)

	chunk, err := store.ListStoredRecommendationsChunk(ctx, config.RecommendationFilter{}, nil, 10)
	require.NoError(t, err)
	assert.Empty(t, chunk.Recs)
	assert.Nil(t, chunk.Last)
	assert.True(t, chunk.Done)
}

// TestListStoredRecommendationsChunk_UsesScanKeyIndex proves the chunk query
// matches the expression migration 000142 indexes: with sequential and bitmap
// scans disabled the plan reads recommendations_scan_key_idx in order and has no
// Sort node. A drift between recommendationScanKeyExpr and the index (for
// example dropping COLLATE "C") brings the full scan plus sort back for every
// chunk.
func TestListStoredRecommendationsChunk_UsesScanKeyIndex(t *testing.T) {
	ctx := context.Background()
	db := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath()).DB.Pool()

	for name, after := range map[string]*config.RecommendationScanKey{
		"first chunk": nil,
		"resume":      {ID: "aws|x", RowID: "00000000-0000-0000-0000-000000000001"},
	} {
		t.Run(name, func(t *testing.T) {
			query, args := config.BuildRecommendationChunkQuery(config.RecommendationFilter{}, after, 500)
			plan := explainWithoutSeqScan(ctx, t, db, query, args)
			assert.Contains(t, plan, "recommendations_scan_key_idx", plan)
			assert.NotContains(t, plan, "Sort", plan)
		})
	}
}

func explainWithoutSeqScan(ctx context.Context, t *testing.T, db interface {
	Begin(context.Context) (pgx.Tx, error)
}, query string, args []any) string {
	t.Helper()
	tx, err := db.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	// An empty test table makes any plan cheap, so steer the planner away
	// from the scans that do not return rows in index order; the question
	// is whether the ordered index scan can serve the query at all.
	_, err = tx.Exec(ctx, `SET LOCAL enable_seqscan = off; SET LOCAL enable_bitmapscan = off`)
	require.NoError(t, err)
	rows, err := tx.Query(ctx, "EXPLAIN "+query, args...)
	require.NoError(t, err)
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		lines = append(lines, line)
	}
	require.NoError(t, rows.Err())
	return strings.Join(lines, "\n")
}

// TestListStoredRecommendationsChunk_ConcurrentUpsertsDuringWalk runs chunk
// walks while collections keep upserting the same rows (new payload values,
// same natural keys) and inserting and evicting other rows. Every row that
// exists for the whole walk must come back exactly once and in order: an
// upsert keeps the primary key, so the (ID, primary key) position stays
// valid between chunks.
func TestListStoredRecommendationsChunk_ConcurrentUpsertsDuringWalk(t *testing.T) {
	ctx := context.Background()
	store := setupRecommendationsStore(ctx, t)

	const stable = 300
	base := make([]config.RecommendationRecord, stable)
	wantStable := make([]string, stable)
	for i := range base {
		base[i] = scanRec(fmt.Sprintf("s%04d", i), fmt.Sprintf("s%04d", i))
		wantStable[i] = base[i].ID
	}
	require.NoError(t, store.UpsertRecommendations(ctx, time.Now().UTC(), base, nil))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	writerErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for round := 0; ; round++ {
			select {
			case <-stop:
				return
			default:
			}
			recs := append([]config.RecommendationRecord(nil), base...)
			for i := range recs {
				recs[i].Savings = float64(round)
			}
			// A churning row that sorts between stable ones.
			recs = append(recs, scanRec(fmt.Sprintf("s%04d-churn-%d", round%stable, round), fmt.Sprintf("c%06d", round)))
			if err := store.UpsertRecommendations(ctx, time.Now().UTC(), recs, []config.SuccessfulCollect{{Provider: "aws"}}); err != nil {
				writerErr <- err
				return
			}
		}
	}()

	for walk := 0; walk < 20; walk++ {
		got, _ := walkChunks(ctx, t, store, config.RecommendationFilter{}, 7)
		assert.True(t, sort.StringsAreSorted(got), "walk %d out of order", walk)
		var gotStable []string
		for _, id := range got {
			if !strings.Contains(id, "-churn-") {
				gotStable = append(gotStable, id)
			}
		}
		require.Equal(t, wantStable, gotStable, "walk %d must return every stable row exactly once", walk)
	}
	close(stop)
	wg.Wait()
	select {
	case err := <-writerErr:
		require.NoError(t, err)
	default:
	}
}
