//go:build integration
// +build integration

package config_test

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireLastCollectedAt asserts recommendations_state.last_collected_at
// equals want.
func requireLastCollectedAt(ctx context.Context, t *testing.T, store *config.PostgresStore, want time.Time) {
	t.Helper()
	fr, err := store.GetRecommendationsFreshness(ctx)
	require.NoError(t, err)
	require.NotNil(t, fr.LastCollectedAt)
	assert.True(t, want.Equal(*fr.LastCollectedAt),
		"last_collected_at = %s, want %s", fr.LastCollectedAt.UTC(), want)
}

// TestPostgresStore_UpsertRecommendations_LastCollectedAtNeverMovesBackwards
// pins issue #111: a collection that started earlier but commits later must
// not move last_collected_at back to its older timestamp.
func TestPostgresStore_UpsertRecommendations_LastCollectedAtNeverMovesBackwards(t *testing.T) {
	ctx := context.Background()
	store := setupRecommendationsStore(ctx, t)

	newer := time.Now().UTC().Truncate(time.Second)
	older := newer.Add(-3 * time.Minute)

	require.NoError(t, store.UpsertRecommendations(ctx, newer, nil, nil))
	requireLastCollectedAt(ctx, t, store, newer)

	require.NoError(t, store.UpsertRecommendations(ctx, older, nil, nil))
	requireLastCollectedAt(ctx, t, store, newer)

	// A genuinely newer collection still advances it.
	newest := newer.Add(time.Minute)
	require.NoError(t, store.UpsertRecommendations(ctx, newest, nil, nil))
	requireLastCollectedAt(ctx, t, store, newest)
}

// TestPostgresStore_UpsertRecommendations_LastCollectedAtConcurrentCommit
// covers the race in issue #111 on real Postgres: the newer collection holds
// the state row's lock and commits while the older one is blocked on it. The
// blocked UPDATE must re-evaluate GREATEST against the committed newer value
// rather than overwrite it with the older timestamp.
func TestPostgresStore_UpsertRecommendations_LastCollectedAtConcurrentCommit(t *testing.T) {
	ctx := context.Background()
	db := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath()).DB
	store := config.NewPostgresStore(db)

	base := time.Now().UTC().Truncate(time.Second)
	older := base.Add(-2 * time.Minute)
	newer := base

	require.NoError(t, store.UpsertRecommendations(ctx, base.Add(-time.Hour), nil, nil))

	// The newer run's state write, held open so it owns the row lock.
	tx, err := db.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	_, err = tx.Exec(ctx, `UPDATE recommendations_state SET last_collected_at = GREATEST(last_collected_at, $1) WHERE id = 1`, newer)
	require.NoError(t, err)

	// The older run commits after it: its UPDATE blocks on the row lock.
	done := make(chan error, 1)
	go func() { done <- store.UpsertRecommendations(ctx, older, nil, nil) }()

	require.Eventually(t, func() bool {
		var waiting int
		if qerr := db.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&waiting); qerr != nil {
			return false
		}
		return waiting > 0
	}, 10*time.Second, 20*time.Millisecond, "older run's UPDATE never blocked on the state row lock")

	require.NoError(t, tx.Commit(ctx))

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("older run's UpsertRecommendations did not finish after the newer run committed")
	}

	requireLastCollectedAt(ctx, t, store, newer)
}
