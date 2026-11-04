//go:build integration
// +build integration

package config_test

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pruneRetentionDays is the retention every prune test runs with; the seeded
// ages below are chosen around it.
const pruneRetentionDays = 30

// azureAccountRec is an Azure rec owned by a registered account.
func azureAccountRec(id, accountID, resourceType string) config.RecommendationRecord {
	acct := accountID
	return config.RecommendationRecord{
		ID:             id,
		CloudAccountID: &acct,
		Provider:       "azure",
		Service:        "vm",
		Region:         "eastus",
		ResourceType:   resourceType,
		Savings:        10,
		Count:          1,
		Term:           12,
		Payment:        "no-upfront",
	}
}

// ambientRec is a rec with no registered account (account_key = zero UUID).
func ambientRec(id, provider, resourceType string) config.RecommendationRecord {
	return config.RecommendationRecord{
		ID:           id,
		Provider:     provider,
		Service:      "vm",
		Region:       "eastus",
		ResourceType: resourceType,
		Savings:      10,
		Count:        1,
		Term:         12,
		Payment:      "no-upfront",
	}
}

// seedAt writes recs stamped with collectedAt and evicts nothing (no
// successful collects), so each call adds rows of the given age.
func seedAt(ctx context.Context, t *testing.T, store *config.PostgresStore, collectedAt time.Time, recs ...config.RecommendationRecord) {
	t.Helper()
	require.NoError(t, store.UpsertRecommendations(ctx, collectedAt, recs, nil))
}

// storedRecIDs returns the sorted payload ids of every stored row.
func storedRecIDs(ctx context.Context, t *testing.T, store *config.PostgresStore) []string {
	t.Helper()
	recs, err := store.ListStoredRecommendations(ctx, config.RecommendationFilter{})
	require.NoError(t, err)
	ids := make([]string, 0, len(recs))
	for i := range recs {
		ids = append(ids, recs[i].ID)
	}
	sort.Strings(ids)
	return ids
}

// TestPostgresStore_PruneStaleRecommendations_PairScope pins issue #120: only
// the rows of a (provider, account) pair with no row inside the window go,
// and a pair with one recent row keeps all of its rows, however old.
func TestPostgresStore_PruneStaleRecommendations_PairScope(t *testing.T) {
	ctx := context.Background()
	store := setupRecommendationsStore(ctx, t)

	staleAcct := uuid.NewString()
	freshAcct := uuid.NewString()
	edgeAcct := uuid.NewString()
	seedRecommendationCloudAccount(ctx, t, store, staleAcct, "azure", "sub-120-stale")
	seedRecommendationCloudAccount(ctx, t, store, freshAcct, "azure", "sub-120-fresh")
	seedRecommendationCloudAccount(ctx, t, store, edgeAcct, "azure", "sub-120-edge")

	now := time.Now().UTC()
	old := now.AddDate(0, 0, -(pruneRetentionDays + 10))
	justOld := now.AddDate(0, 0, -(pruneRetentionDays + 1))
	recent := now.Add(-time.Hour)
	insideWindow := now.AddDate(0, 0, -(pruneRetentionDays - 1))

	// Stale registered pair: every row is outside the window.
	seedAt(ctx, t, store, old, azureAccountRec("stale-1", staleAcct, "D2"), azureAccountRec("stale-2", staleAcct, "D4"))
	// Fresh registered pair with mixed ages: one recent row keeps the others.
	seedAt(ctx, t, store, recent, azureAccountRec("fresh-new", freshAcct, "D2"))
	seedAt(ctx, t, store, old, azureAccountRec("fresh-old", freshAcct, "D4"))
	seedAt(ctx, t, store, justOld, azureAccountRec("fresh-older", freshAcct, "D8"))
	// A pair whose newest row is a day inside the window is not stale yet.
	seedAt(ctx, t, store, insideWindow, azureAccountRec("edge-1", edgeAcct, "D2"))
	// Ambient pairs: the fresh azure ambient pair must not shield the stale
	// aws ambient pair (provider is part of the pair) nor the stale azure
	// registered pair above (the account is part of the pair).
	seedAt(ctx, t, store, old, ambientRec("aws-ambient-old", "aws", "m5.large"))
	seedAt(ctx, t, store, recent, ambientRec("azure-ambient-new", "azure", "D16"))

	deleted, err := store.PruneStaleRecommendations(ctx, pruneRetentionDays)
	require.NoError(t, err)
	assert.Equal(t, int64(3), deleted, "two stale registered rows and one stale ambient row")
	assert.Equal(t,
		[]string{"azure-ambient-new", "edge-1", "fresh-new", "fresh-old", "fresh-older"},
		storedRecIDs(ctx, t, store))

	// Idempotent: nothing left to prune.
	deleted, err = store.PruneStaleRecommendations(ctx, pruneRetentionDays)
	require.NoError(t, err)
	assert.Zero(t, deleted)
}

// A non-positive retention is refused rather than read as "everything is
// stale".
func TestPostgresStore_PruneStaleRecommendations_RejectsNonPositiveRetention(t *testing.T) {
	ctx := context.Background()
	store := setupRecommendationsStore(ctx, t)
	seedAt(ctx, t, store, time.Now().UTC().AddDate(0, 0, -400), ambientRec("ancient", "aws", "m5.large"))

	for _, days := range []int{0, -1} {
		deleted, err := store.PruneStaleRecommendations(ctx, days)
		require.Error(t, err, "retention %d", days)
		assert.Zero(t, deleted)
	}
	assert.Equal(t, []string{"ancient"}, storedRecIDs(ctx, t, store))
}

// waitForLockWaiters blocks until at least n backends of this test's database
// are waiting on a heavyweight lock.
func waitForLockWaiters(ctx context.Context, t *testing.T, db *database.Connection, n int, what string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting int
		if err := db.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database()
			   AND wait_event_type = 'Lock'
		`).Scan(&waiting); err != nil {
			return false
		}
		return waiting >= n
	}, 10*time.Second, 20*time.Millisecond, what)
}

// TestPostgresStore_PruneStaleRecommendations_ConcurrentCollection runs the
// prune while a collection that refreshes a stale pair is still in flight. The
// collection has already upserted one of the pair's rows (holding its row
// lock) and is held open on the recommendations_state row lock. The prune must
// wait for it and then treat the pair as fresh: it keeps both the refreshed
// row and the pair's other, older row, while a pair nobody collected is still
// pruned.
func TestPostgresStore_PruneStaleRecommendations_ConcurrentCollection(t *testing.T) {
	ctx := context.Background()
	db := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath()).DB
	store := config.NewPostgresStore(db)

	pairAcct := uuid.NewString()
	goneAcct := uuid.NewString()
	seedRecommendationCloudAccount(ctx, t, store, pairAcct, "azure", "sub-120-pair")
	seedRecommendationCloudAccount(ctx, t, store, goneAcct, "azure", "sub-120-gone")

	old := time.Now().UTC().AddDate(0, 0, -(pruneRetentionDays + 10))
	seedAt(ctx, t, store, old,
		azureAccountRec("pair-refreshed", pairAcct, "D2"),
		azureAccountRec("pair-untouched", pairAcct, "D4"),
		azureAccountRec("gone-1", goneAcct, "D2"))

	// Hold the state row so the collection below blocks on its final write,
	// after its upsert has taken the row lock on pair-refreshed.
	hold, err := db.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = hold.Rollback(context.Background()) })
	_, err = hold.Exec(ctx, `SELECT 1 FROM recommendations_state WHERE id = 1 FOR UPDATE`)
	require.NoError(t, err)

	// A partial collection for the pair: it refreshes one row and, because the
	// pair is not in successfulCollects, leaves the other row in place.
	collectedAt := time.Now().UTC()
	collectDone := make(chan error, 1)
	go func() {
		collectDone <- store.UpsertRecommendations(ctx, collectedAt,
			[]config.RecommendationRecord{azureAccountRec("pair-refreshed", pairAcct, "D2")}, nil)
	}()
	waitForLockWaiters(ctx, t, db, 1, "collection never blocked on the held state row")

	type pruneResult struct {
		deleted int64
		err     error
	}
	pruneDone := make(chan pruneResult, 1)
	go func() {
		n, perr := store.PruneStaleRecommendations(ctx, pruneRetentionDays)
		pruneDone <- pruneResult{n, perr}
	}()
	waitForLockWaiters(ctx, t, db, 2, "prune never waited on the in-flight collection")

	require.NoError(t, hold.Commit(ctx))

	select {
	case cerr := <-collectDone:
		require.NoError(t, cerr)
	case <-time.After(10 * time.Second):
		t.Fatal("collection did not finish after the state row was released")
	}
	var res pruneResult
	select {
	case res = <-pruneDone:
		require.NoError(t, res.err)
	case <-time.After(10 * time.Second):
		t.Fatal("prune did not finish after the collection committed")
	}

	assert.Equal(t, int64(1), res.deleted, "only the uncollected pair's row is pruned")
	assert.Equal(t, []string{"pair-refreshed", "pair-untouched"}, storedRecIDs(ctx, t, store))

	var stamped time.Time
	require.NoError(t, db.QueryRow(ctx,
		`SELECT collected_at FROM recommendations WHERE payload->>'id' = 'pair-refreshed'`).Scan(&stamped))
	assert.WithinDuration(t, collectedAt, stamped, time.Second, "the refreshed row keeps the collection's timestamp")
}

// TestPostgresStore_PruneStaleRecommendations_RacingCollections races prunes
// against collections that refresh a stale pair, in no fixed order. Whichever
// commits first, the refreshed row must survive: a prune that runs first
// deletes the old rows and the collection re-inserts, and a prune that runs
// second sees the pair as fresh.
func TestPostgresStore_PruneStaleRecommendations_RacingCollections(t *testing.T) {
	ctx := context.Background()
	store := setupRecommendationsStore(ctx, t)
	acct := uuid.NewString()
	seedRecommendationCloudAccount(ctx, t, store, acct, "azure", "sub-120-race")

	for round := 0; round < 20; round++ {
		old := time.Now().UTC().AddDate(0, 0, -(pruneRetentionDays + 10))
		seedAt(ctx, t, store, old, azureAccountRec("race-row", acct, "D2"))

		start := make(chan struct{})
		collectDone := make(chan error, 1)
		pruneDone := make(chan error, 1)
		go func() {
			<-start
			collectDone <- store.UpsertRecommendations(ctx, time.Now().UTC(),
				[]config.RecommendationRecord{azureAccountRec("race-row", acct, "D2")},
				[]config.SuccessfulCollect{{Provider: "azure", CloudAccountID: &acct}})
		}()
		go func() {
			<-start
			_, perr := store.PruneStaleRecommendations(ctx, pruneRetentionDays)
			pruneDone <- perr
		}()
		close(start)
		require.NoError(t, <-collectDone, "round %d", round)
		require.NoError(t, <-pruneDone, "round %d", round)

		require.Equal(t, []string{"race-row"}, storedRecIDs(ctx, t, store), "round %d: refreshed row lost", round)
	}
}
