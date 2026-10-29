package config

import (
	"context"
	"fmt"
)

// PruneStaleRecommendations deletes the cached recommendations of every
// (provider, account) pair that has not collected for retentionDays, and
// returns how many rows it deleted (issue #120).
//
// UpsertRecommendations only evicts rows of the pairs that collected
// successfully in the current run, so the rows of a pair that never collects
// again (its provider was disabled, its account disabled, or its credentials
// keep failing) would otherwise stay on the Recommendations page forever.
// Registered accounts that are deleted outright are already covered: the
// cloud_account_id FK cascades.
//
// A pair's newest row is its last collection: every collection stamps the
// rows it upserts with its own collected_at, and a successful one evicts the
// pair's older rows. A pair counts as stale only when it has no row at all
// collected inside the window, and only rows of such pairs are deleted. A pair
// with even one recent row keeps every row, however old the rest are (a
// partial collection leaves the failed services' rows behind on purpose).
//
// Concurrent collections: the table lock below (SHARE ROW EXCLUSIVE conflicts
// with the ROW EXCLUSIVE lock every INSERT, UPDATE and DELETE takes, and with
// itself) makes the prune wait for any UpsertRecommendations transaction still
// in flight, and makes collections that start meanwhile wait for the prune.
// The DELETE's snapshot is taken after the lock is granted, so a collection
// that committed fresh rows for a pair always counts, and no collection can
// commit between the pair check and the delete. Readers take ACCESS SHARE,
// which this lock does not block. The prune holds no other lock before
// requesting this one, and nothing in the codebase row-locks recommendations
// without writing them, so the wait cannot deadlock.
//
// The row-level collected_at bound is implied by the NOT EXISTS (every row of
// a stale pair is older than the cutoff); it is kept so the planner can drive
// the delete from recommendations_collected_idx rather than scan the table.
func (s *PostgresStore) PruneStaleRecommendations(ctx context.Context, retentionDays int) (int64, error) {
	if retentionDays <= 0 {
		return 0, fmt.Errorf("prune stale recommendations: retention must be at least 1 day, got %d", retentionDays)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("prune stale recommendations: begin tx: %w", err)
	}
	defer rollbackTx(ctx, tx, "PruneStaleRecommendations")

	if _, err = tx.Exec(ctx, `LOCK TABLE recommendations IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return 0, fmt.Errorf("prune stale recommendations: lock table: %w", err)
	}

	tag, err := tx.Exec(ctx, `
		DELETE FROM recommendations r
		 WHERE r.collected_at < NOW() - INTERVAL '1 day' * $1
		   AND NOT EXISTS (
		       SELECT 1
		         FROM recommendations fresh
		        WHERE fresh.provider     = r.provider
		          AND fresh.account_key  = r.account_key
		          AND fresh.collected_at >= NOW() - INTERVAL '1 day' * $1
		   )
	`, retentionDays)
	if err != nil {
		return 0, fmt.Errorf("prune stale recommendations: delete: %w", err)
	}

	if err = tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("prune stale recommendations: commit: %w", err)
	}
	return tag.RowsAffected(), nil
}
