//go:build integration
// +build integration

package database_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runLockedTasks starts tasks goroutines that each do what
// server.HandleScheduledTask does: pin a pooled connection with
// TryAdvisoryLock (a distinct lock per task type), then query through the
// pool while still holding it. All locks are taken before any task queries,
// which is the worst interleaving. It returns how many queries succeeded
// before the deadline.
func runLockedTasks(t *testing.T, db *database.Connection, tasks int) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var locked, done sync.WaitGroup
	locked.Add(tasks)
	results := make(chan error, tasks)
	for i := 0; i < tasks; i++ {
		done.Add(1)
		go func(lockID int64) {
			defer done.Done()
			acquired, err := db.TryAdvisoryLock(ctx, lockID)
			locked.Done()
			if err != nil || !acquired {
				results <- assert.AnError
				return
			}
			defer db.ReleaseAdvisoryLock(context.Background(), lockID)
			locked.Wait()
			var one int
			results <- db.QueryRow(ctx, "SELECT 1").Scan(&one)
		}(int64(718_000 + i))
	}
	done.Wait()
	close(results)

	ok := 0
	for err := range results {
		if err == nil {
			ok++
		}
	}
	return ok
}

// TestPoolSizeFloorForAdvisoryLockedTasks pins why terraform's
// db_max_connections validation rejects a pool below 2 (#718): a scheduled
// task holds one pooled connection for its advisory lock while it queries
// through the same pool, so N tasks running at once on one instance need a
// pool of at least N+1. At exactly N the pool is exhausted by the pinned
// connections and every task waits on itself until its deadline.
func TestPoolSizeFloorForAdvisoryLockedTasks(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { _ = container.Cleanup(context.Background()) })

	cases := []struct {
		name        string
		pool, tasks int
		wantOK      int
	}{
		{"pool 1 deadlocks a single task", 1, 1, 0},
		{"pool 2 serves a single task", 2, 1, 1},
		{"pool 6 serves 5 concurrent tasks", 6, 5, 5},
		{"pool 6 deadlocks 6 concurrent tasks", 6, 6, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := *container.Config
			cfg.MaxConnections = tc.pool
			cfg.MinConnections = 0
			db, err := database.NewConnection(ctx, &cfg, nil)
			require.NoError(t, err)
			defer db.Close()

			assert.Equal(t, tc.wantOK, runLockedTasks(t, db, tc.tasks),
				"pool %d with %d advisory-locked tasks", tc.pool, tc.tasks)
		})
	}
}
