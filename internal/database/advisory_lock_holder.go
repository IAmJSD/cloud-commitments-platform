package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// advisoryLockHolderAgeSQL returns how long the session holding the
// session-level advisory lock $1 in the current database has been in its
// current state.
//
// pg_locks splits a bigint advisory key into classid (high 32 bits) and objid
// (low 32 bits) with objsubid = 1; the shift-and-or reassembles the original
// signed int64 so negative keys (half of all FNV-derived task lock IDs) match.
//
// For a lock taken by TryAdvisoryLock the pinned holder session runs nothing
// between pg_try_advisory_lock and pg_advisory_unlock, so it sits idle from
// acquisition onward and state_change is the acquisition time. The age is
// computed server-side so client/server clock skew cannot distort it.
//
// Rows whose state_change is NULL (a session owned by another role, visible
// without pg_read_all_stats) are filtered out, because their age is unknown.
const advisoryLockHolderAgeSQL = `
SELECT EXTRACT(EPOCH FROM (clock_timestamp() - a.state_change))::float8
FROM pg_locks l
JOIN pg_stat_activity a ON a.pid = l.pid
WHERE l.locktype = 'advisory'
  AND l.granted
  AND l.objsubid = 1
  AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND ((l.classid::bigint << 32) | l.objid::bigint) = $1
  AND a.state_change IS NOT NULL
LIMIT 1`

// AdvisoryLockHeldFor reports how long another session has held the
// session-level advisory lock lockID. found is false when no session holds it
// (for example it was released between a failed TryAdvisoryLock and this
// call) or when the holder's age is not visible to this role.
//
// The age is exact for locks taken through TryAdvisoryLock (see
// advisoryLockHolderAgeSQL). It under-reports for a lock stranded on a
// connection that went back to the pool and keeps running other queries,
// since each query refreshes state_change; ReleaseAdvisoryLock closes such
// connections instead of pooling them (issue #105), so that case should not
// arise. Read-only and safe to call concurrently.
func (c *Connection) AdvisoryLockHeldFor(ctx context.Context, lockID int64) (time.Duration, bool, error) {
	var seconds float64
	err := c.pool.QueryRow(ctx, advisoryLockHolderAgeSQL, lockID).Scan(&seconds)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("failed to read advisory lock %d holder age: %w", lockID, err)
	}
	return time.Duration(seconds * float64(time.Second)), true, nil
}
