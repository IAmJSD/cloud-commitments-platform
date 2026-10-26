//go:build integration
// +build integration

package config_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// collectionMarker is the raw recommendations_state marker, read straight
// from the row because GetRecommendationsFreshness does not expose the owner.
type collectionMarker struct {
	startedAt *time.Time
	owner     *string
}

func readCollectionMarker(ctx context.Context, t *testing.T, pool *pgxpool.Pool) collectionMarker {
	t.Helper()
	var m collectionMarker
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT last_collection_started_at, last_collection_owner_id::text
		  FROM recommendations_state
		 WHERE id = 1
	`).Scan(&m.startedAt, &m.owner))
	return m
}

func resetCollectionMarker(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		UPDATE recommendations_state
		   SET last_collection_started_at = NULL,
		       last_collection_owner_id    = NULL
		 WHERE id = 1
	`)
	require.NoError(t, err)
}

func markCollection(ctx context.Context, t *testing.T, store *config.PostgresStore) string {
	t.Helper()
	token, ok, err := store.MarkCollectionStarted(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	return token
}

// TestPostgresStore_ClaimCollectionInvocation pins issue #84 at the store
// level. The marker token MarkCollectionStarted hands out is shared by every
// Lambda delivery of the refresh's async event; the delivery that wins the
// task advisory lock re-stamps the marker with a fresh invocation token so a
// lock-skipped duplicate's ClearCollectionStarted(markerToken) no longer
// matches it.
func TestPostgresStore_ClaimCollectionInvocation(t *testing.T) {
	ctx := context.Background()
	db := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath()).DB
	pool := db.Pool()
	store := config.NewPostgresStore(db)

	t.Run("owned marker is re-stamped and the duplicate's clear no longer matches", func(t *testing.T) {
		resetCollectionMarker(ctx, t, pool)
		markerToken := markCollection(ctx, t, store)
		before := readCollectionMarker(ctx, t, pool)

		invocationToken, ok, err := store.ClaimCollectionInvocation(ctx, markerToken)
		require.NoError(t, err)
		require.True(t, ok)
		_, parseErr := uuid.Parse(invocationToken)
		require.NoError(t, parseErr, "invocation token must be a UUID")
		require.NotEqual(t, markerToken, invocationToken)

		after := readCollectionMarker(ctx, t, pool)
		require.NotNil(t, after.owner)
		assert.Equal(t, invocationToken, *after.owner)
		require.NotNil(t, after.startedAt)
		assert.True(t, before.startedAt.Equal(*after.startedAt),
			"re-stamping an owned marker must keep the started_at the refresh recorded")

		// The duplicate delivery, lock-skipped, releases with the marker token.
		require.NoError(t, store.ClearCollectionStarted(ctx, markerToken))
		assert.NotNil(t, readCollectionMarker(ctx, t, pool).startedAt,
			"the duplicate's marker-token clear must not release the winner's marker")

		// The winner's deferred clear with its invocation token releases it.
		require.NoError(t, store.ClearCollectionStarted(ctx, invocationToken))
		m := readCollectionMarker(ctx, t, pool)
		assert.Nil(t, m.startedAt)
		assert.Nil(t, m.owner)
	})

	t.Run("marker already cleared by the duplicate is re-established", func(t *testing.T) {
		resetCollectionMarker(ctx, t, pool)
		markerToken := markCollection(ctx, t, store)
		require.NoError(t, store.ClearCollectionStarted(ctx, markerToken))

		invocationToken, ok, err := store.ClaimCollectionInvocation(ctx, markerToken)
		require.NoError(t, err)
		require.True(t, ok)
		m := readCollectionMarker(ctx, t, pool)
		require.NotNil(t, m.startedAt, "the lock winner must re-establish a cleared marker")
		require.NotNil(t, m.owner)
		assert.Equal(t, invocationToken, *m.owner)
		assert.WithinDuration(t, time.Now(), *m.startedAt, time.Minute)
	})

	t.Run("fresh marker owned by another run is left alone", func(t *testing.T) {
		resetCollectionMarker(ctx, t, pool)
		markerToken := markCollection(ctx, t, store)
		require.NoError(t, store.ClearCollectionStarted(ctx, markerToken))
		otherToken := markCollection(ctx, t, store)

		token, ok, err := store.ClaimCollectionInvocation(ctx, markerToken)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Empty(t, token)
		m := readCollectionMarker(ctx, t, pool)
		require.NotNil(t, m.owner)
		assert.Equal(t, otherToken, *m.owner, "another run's fresh marker must keep its owner")
	})

	t.Run("stale marker owned by another run is taken over", func(t *testing.T) {
		resetCollectionMarker(ctx, t, pool)
		markerToken := markCollection(ctx, t, store)
		require.NoError(t, store.ClearCollectionStarted(ctx, markerToken))
		markCollection(ctx, t, store)
		_, err := pool.Exec(ctx, `
			UPDATE recommendations_state
			   SET last_collection_started_at = NOW() - INTERVAL '6 minutes'
			 WHERE id = 1
		`)
		require.NoError(t, err)

		invocationToken, ok, err := store.ClaimCollectionInvocation(ctx, markerToken)
		require.NoError(t, err)
		require.True(t, ok)
		m := readCollectionMarker(ctx, t, pool)
		require.NotNil(t, m.owner)
		assert.Equal(t, invocationToken, *m.owner)
		assert.WithinDuration(t, time.Now(), *m.startedAt, time.Minute,
			"taking over a stale marker must restart its 5-minute window")
	})

	t.Run("empty marker token is a boundary error", func(t *testing.T) {
		_, ok, err := store.ClaimCollectionInvocation(ctx, "")
		require.Error(t, err)
		assert.False(t, ok)
	})
}

// TestPostgresStore_ClaimCollectionInvocation_Races runs the issue #84
// interleavings concurrently against real Postgres, where the two UPDATEs
// on recommendations_state row 1 serialize on its row lock and the loser
// re-evaluates its WHERE clause against the winner's committed row.
func TestPostgresStore_ClaimCollectionInvocation_Races(t *testing.T) {
	ctx := context.Background()
	db := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath()).DB
	pool := db.Pool()
	store := config.NewPostgresStore(db)

	// The lock winner's claim races the lock-skipped duplicate's clear, both
	// with the shared marker token. Whichever commits first, the winner must
	// end up owning a set marker: claim-first makes the clear a no-op, and
	// clear-first leaves a free marker the claim re-establishes.
	t.Run("winner's claim against the duplicate's clear", func(t *testing.T) {
		claimFirst, clearFirst := 0, 0
		for i := 0; i < 50; i++ {
			resetCollectionMarker(ctx, t, pool)
			markerToken := markCollection(ctx, t, store)
			markedAt := readCollectionMarker(ctx, t, pool).startedAt

			var (
				wg                 sync.WaitGroup
				invocationToken    string
				claimed            bool
				claimErr, clearErr error
				start              = make(chan struct{})
			)
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				invocationToken, claimed, claimErr = store.ClaimCollectionInvocation(ctx, markerToken)
			}()
			go func() {
				defer wg.Done()
				<-start
				clearErr = store.ClearCollectionStarted(ctx, markerToken)
			}()
			close(start)
			wg.Wait()

			require.NoError(t, claimErr)
			require.NoError(t, clearErr)
			require.True(t, claimed, "iteration %d: the lock winner must own the marker", i)
			m := readCollectionMarker(ctx, t, pool)
			require.NotNil(t, m.startedAt, "iteration %d: marker must be set while the winner collects", i)
			require.NotNil(t, m.owner)
			require.Equal(t, invocationToken, *m.owner, "iteration %d", i)
			// An unchanged started_at means the claim re-stamped the owned
			// marker; a new one means the clear landed first and the claim
			// re-established it.
			if m.startedAt.Equal(*markedAt) {
				claimFirst++
			} else {
				clearFirst++
			}
		}
		t.Logf("orderings observed: claim-first=%d clear-first=%d", claimFirst, clearFirst)
	})

	// Concurrent claims with the same marker token: exactly one becomes the
	// owner. Production only claims under the advisory lock, so this is a
	// defense-in-depth check that the UPDATE itself is single-winner.
	t.Run("concurrent claims elect one owner", func(t *testing.T) {
		const claimers = 8
		for i := 0; i < 20; i++ {
			resetCollectionMarker(ctx, t, pool)
			markerToken := markCollection(ctx, t, store)

			var (
				wg     sync.WaitGroup
				mu     sync.Mutex
				owners []string
				errs   []error
				start  = make(chan struct{})
			)
			for c := 0; c < claimers; c++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					token, ok, err := store.ClaimCollectionInvocation(ctx, markerToken)
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						errs = append(errs, err)
					}
					if ok {
						owners = append(owners, token)
					}
				}()
			}
			close(start)
			wg.Wait()

			require.Empty(t, errs)
			require.Len(t, owners, 1, "iteration %d: exactly one claim may win", i)
			m := readCollectionMarker(ctx, t, pool)
			require.NotNil(t, m.owner)
			require.Equal(t, owners[0], *m.owner)
		}
	})
}
