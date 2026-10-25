//go:build integration
// +build integration

package database_test

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReleaseAdvisoryLock_DetachesFromCanceledCallerContext pins issue #105:
// ReleaseAdvisoryLock must actually release the session-level advisory lock
// even when the caller's context is already canceled or expired by the time
// the deferred release runs -- exactly what happens when a scheduled task's
// invocation context (Lambda deadline, or the HTTP request context) expires
// while dispatchTask is still running.
//
// Pre-fix, the unlock query ran on the caller's own ctx: a dead ctx made
// `SELECT pg_advisory_unlock($1)` fail immediately with "context canceled",
// which was only logged as a warning, and the otherwise-healthy pinned
// connection still went back to the pool via the deferred conn.Release() --
// with the session-level lock still held on that backend session. Every
// subsequent TryAdvisoryLock for that task type would then see the lock
// held by that (still-alive, pooled) session and report "already_running"
// (HTTP 200) until that specific connection happened to be recycled,
// silently and indefinitely stalling the task.
//
// Verification uses a second, independent raw connection rather than
// container.DB's own pool: pg_try_advisory_lock is reentrant per session
// (a session re-acquiring a lock it already holds succeeds trivially), so
// asking container.DB.TryAdvisoryLock again could spuriously pass by
// reacquiring the very same pooled backend connection that never released
// the lock in the first place.
func TestReleaseAdvisoryLock_DetachesFromCanceledCallerContext(t *testing.T) {
	ctx := context.Background()
	// Empty, not migrated: advisory lock keys are per database, so another
	// database on the shared server cannot hold this one's lock.
	container := testhelpers.RequireEmptyDatabase(ctx, t)

	const lockID int64 = 424242424242

	acquired, err := container.DB.TryAdvisoryLock(ctx, lockID)
	require.NoError(t, err)
	require.True(t, acquired, "must win the lock against a fresh session")

	// Simulate the caller's context already being dead by the time the
	// deferred ReleaseAdvisoryLock call runs.
	deadCtx, cancel := context.WithCancel(ctx)
	cancel()

	container.DB.ReleaseAdvisoryLock(deadCtx, lockID)

	// Verify from an independent session: if the release actually ran
	// (detached from deadCtx as it must), a different backend session can
	// take the lock immediately.
	verifyConn, err := pgx.Connect(ctx, container.Config.DSN(container.Config.Password))
	require.NoError(t, err)
	defer func() { _ = verifyConn.Close(context.Background()) }()

	verifyCtx, verifyCancel := context.WithTimeout(ctx, 5*time.Second)
	defer verifyCancel()

	var reacquired bool
	err = verifyConn.QueryRow(verifyCtx, "SELECT pg_try_advisory_lock($1)", lockID).Scan(&reacquired)
	require.NoError(t, err)
	assert.True(t, reacquired,
		"a different session must be able to take the lock: ReleaseAdvisoryLock must not skip "+
			"the unlock just because the caller's context is already canceled")

	if reacquired {
		_, _ = verifyConn.Exec(verifyCtx, "SELECT pg_advisory_unlock($1)", lockID)
	}
}
