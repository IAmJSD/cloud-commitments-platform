//go:build integration
// +build integration

package database_test

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAdvisoryLockHeldFor checks, against a real PostgreSQL, the holder-age
// query HandleScheduledTask uses to tell a stuck task lock from a run that is
// genuinely in progress (issue #394 item 4). The holder is always a separate
// raw session, matching production, where the skipping invocation never holds
// the lock it is asking about.
func TestAdvisoryLockHeldFor(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { _ = container.Cleanup(context.Background()) })

	dsn := container.Config.DSN(container.Config.Password)
	holder, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Close(context.Background()) })

	lock := func(t *testing.T, id int64) {
		t.Helper()
		var ok bool
		require.NoError(t, holder.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", id).Scan(&ok))
		require.True(t, ok)
	}
	unlock := func(t *testing.T, id int64) {
		t.Helper()
		var ok bool
		require.NoError(t, holder.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", id).Scan(&ok))
		require.True(t, ok)
	}

	t.Run("not held", func(t *testing.T) {
		_, found, err := container.DB.AdvisoryLockHeldFor(ctx, 777)
		require.NoError(t, err)
		assert.False(t, found)
	})

	// FNV-derived task lock IDs are negative about half the time, so the
	// classid/objid reassembly must round-trip the full signed range.
	for _, id := range []int64{1, -1, 424242424242, -6148914691236517206, math.MaxInt64, math.MinInt64} {
		t.Run(fmt.Sprintf("round-trips key %d", id), func(t *testing.T) {
			lock(t, id)
			defer unlock(t, id)

			heldFor, found, err := container.DB.AdvisoryLockHeldFor(ctx, id)
			require.NoError(t, err)
			require.True(t, found, "lock %d must be found", id)
			assert.Less(t, heldFor, 30*time.Second)
		})
	}

	t.Run("same low half, different high half does not match", func(t *testing.T) {
		const held int64 = 1<<32 | 5
		lock(t, held)
		defer unlock(t, held)

		_, found, err := container.DB.AdvisoryLockHeldFor(ctx, 5)
		require.NoError(t, err)
		assert.False(t, found)
	})

	// The two-int4 advisory form shows objsubid = 2 and must not be mistaken
	// for the bigint key with the same halves.
	t.Run("two-int4 key form is ignored", func(t *testing.T) {
		_, err := holder.Exec(ctx, "SELECT pg_advisory_lock(0, 9)")
		require.NoError(t, err)
		defer func() { _, _ = holder.Exec(ctx, "SELECT pg_advisory_unlock(0, 9)") }()

		_, found, err := container.DB.AdvisoryLockHeldFor(ctx, 9)
		require.NoError(t, err)
		assert.False(t, found)
	})

	t.Run("age grows from acquisition and clears on release", func(t *testing.T) {
		const id int64 = -5150
		lock(t, id)
		time.Sleep(1200 * time.Millisecond)

		heldFor, found, err := container.DB.AdvisoryLockHeldFor(ctx, id)
		require.NoError(t, err)
		require.True(t, found)
		assert.GreaterOrEqual(t, heldFor, time.Second)
		assert.Less(t, heldFor, 30*time.Second)

		unlock(t, id)
		_, found, err = container.DB.AdvisoryLockHeldFor(ctx, id)
		require.NoError(t, err)
		assert.False(t, found)
	})

	// A lock taken through Connection.TryAdvisoryLock (the production path)
	// is seen from the same pool, since the check runs on another pooled
	// session than the pinned holder.
	t.Run("lock taken by TryAdvisoryLock", func(t *testing.T) {
		const id int64 = -8080
		acquired, err := container.DB.TryAdvisoryLock(ctx, id)
		require.NoError(t, err)
		require.True(t, acquired)
		// Release on every exit: a pinned connection that is never returned
		// makes the pool Close in the container cleanup block forever.
		released := false
		defer func() {
			if !released {
				container.DB.ReleaseAdvisoryLock(ctx, id)
			}
		}()

		_, found, err := container.DB.AdvisoryLockHeldFor(ctx, id)
		require.NoError(t, err)
		assert.True(t, found)

		container.DB.ReleaseAdvisoryLock(ctx, id)
		released = true
		_, found, err = container.DB.AdvisoryLockHeldFor(ctx, id)
		require.NoError(t, err)
		assert.False(t, found)
	})

	// Overlapping skipped invocations check the age at the same time the
	// holder may be releasing. The query is read-only, so every caller must
	// get a clean answer (found or not), never an error.
	t.Run("concurrent checks racing a release", func(t *testing.T) {
		const id int64 = -9090
		lock(t, id)

		var wg sync.WaitGroup
		errs := make(chan error, 32)
		for range 32 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _, err := container.DB.AdvisoryLockHeldFor(ctx, id)
				errs <- err
			}()
		}
		unlock(t, id)
		wg.Wait()
		close(errs)
		for err := range errs {
			assert.NoError(t, err)
		}

		_, found, err := container.DB.AdvisoryLockHeldFor(ctx, id)
		require.NoError(t, err)
		assert.False(t, found)
	})
}
