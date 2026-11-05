package config

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// StampResult is the outcome of StampFirstDispatchTx. At is nil when no stamp
// exists and none may be minted; WithinHorizon is judged on the database
// clock and is false when At is nil.
type StampResult struct {
	At            *time.Time
	WithinHorizon bool
}

// StampFirstDispatchTx stamps a lineage's first dispatch time on the claimed
// running row, with the database clock, and returns it. An existing stamp is
// kept. A stamp is minted only at attempt 0, or when the predecessor is a
// no_submission attempt 0 (the whole lineage never submitted); otherwise a
// stamp-less successor gets At == nil and the caller must refuse it. A row
// that is not running returns ErrExecutionNotInExpectedStatus.
//
// It takes a tx so the dispatch gate can stamp and check conflicts atomically.
func (s *PostgresStore) StampFirstDispatchTx(ctx context.Context, tx pgx.Tx, executionID string, horizon time.Duration) (StampResult, error) {
	var res StampResult
	var within *bool
	err := tx.QueryRow(ctx, `
		UPDATE purchase_executions
		SET first_dispatch_at = COALESCE(first_dispatch_at, CASE
			WHEN retry_attempt_n = 0 OR EXISTS (
				SELECT 1 FROM purchase_executions p
				WHERE p.retry_execution_id = purchase_executions.execution_id
				  AND p.no_submission AND p.retry_attempt_n = 0
			) THEN now() END)
		WHERE execution_id = $1 AND status = 'running'
		RETURNING first_dispatch_at, first_dispatch_at > now() - make_interval(secs => $2)`,
		executionID, horizon.Seconds()).Scan(&res.At, &within)
	if errors.Is(err, pgx.ErrNoRows) {
		return StampResult{}, fmt.Errorf("%w: execution %s is not running", ErrExecutionNotInExpectedStatus, executionID)
	}
	if err != nil {
		return StampResult{}, fmt.Errorf("stamp first dispatch of execution %s: %w", executionID, err)
	}
	res.WithinHorizon = within != nil && *within
	return res, nil
}
