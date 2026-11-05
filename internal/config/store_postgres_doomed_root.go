package config

import (
	"context"
	"fmt"
	"time"
)

// DoomedRoot names the next ramp step whose empty root row the notification
// tick can never fill. Returned by GetDoomedNextRoots and rendered by the
// plan-health step_will_not_attach factor.
type DoomedRoot struct {
	StepNumber int
}

// doomedNextRootQuery finds, per plan, the root the tick would adopt for the
// next step (the same row purchase.adoptableRootRow picks: no account, first
// attempt, pending or notified) when it carries no recommendations and its
// date makes attaching impossible.
//
// Empty means '[]' or jsonb 'null': Add Purchases roots store 'null' because
// json.Marshal(nil) is "null" (store_postgres.go, SavePurchaseExecutionTx).
//
// Doomed is: the date is still ahead ($1 <= scheduled_date; passed dates are
// the overdue factor's job, so the two penalties do not stack) and either the
// plan has no notice window (notification_days_before < 1: attach refuses every
// tick via RequireVetoWindow, so every future empty root is lost) or the date
// is inside it. The window end is $1 + N x 24h, the SQL twin of
// (*PurchasePlan).VetoWindowEnd. Every pooled session is UTC
// (database/connection.go buildPoolConfig forces timezone=UTC), so these fixed
// hours cannot differ from calendar days across a DST change.
//
// dates_managed plans are excluded: their roots are owned by the editor, which
// reschedules them.
const doomedNextRootQuery = `
	SELECT DISTINCT p.id, e.step_number
	  FROM purchase_plans p
	  JOIN purchase_executions e
	    ON e.plan_id = p.id
	   AND e.step_number = COALESCE((p.ramp_schedule ->> 'current_step')::int, 0) + 1
	 WHERE e.cloud_account_id IS NULL
	   AND e.retry_attempt_n = 0
	   AND e.status IN ('pending', 'notified')
	   AND e.recommendations IN ('[]'::jsonb, 'null'::jsonb)
	   AND COALESCE(p.ramp_schedule ->> 'dates_managed', 'false') <> 'true'
	   AND $1::timestamptz <= e.scheduled_date
	   AND (p.notification_days_before < 1
	        OR e.scheduled_date < $1::timestamptz + make_interval(hours => 24 * p.notification_days_before))`

// GetDoomedNextRoots returns, keyed by plan ID, the plans whose next empty root
// is certainly doomed at now. A root dated in the last slice of the window the
// tick may still fill is deliberately not reported. Plans with nothing doomed
// are absent from the map.
func (s *PostgresStore) GetDoomedNextRoots(ctx context.Context, now time.Time) (map[string]DoomedRoot, error) {
	rows, err := s.db.Query(ctx, doomedNextRootQuery, now)
	if err != nil {
		return nil, fmt.Errorf("failed to query doomed next roots: %w", err)
	}
	defer rows.Close()

	doomed := make(map[string]DoomedRoot)
	for rows.Next() {
		var planID string
		var root DoomedRoot
		if err := rows.Scan(&planID, &root.StepNumber); err != nil {
			return nil, fmt.Errorf("failed to scan doomed next root: %w", err)
		}
		doomed[planID] = root
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read doomed next roots: %w", err)
	}
	return doomed, nil
}
