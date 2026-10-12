package config

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// ErrExecutionRetryLinked is returned by RecoverReapedExecutionTx when the
// reaped row already has a retry successor linked. Overwriting it could mean
// two live purchases for one logical step, so the caller must fail loud and
// flag a possible double buy instead of recovering.
var ErrExecutionRetryLinked = errors.New("execution already has a retry successor")

// lockExecutionRow takes a row lock on the execution and returns its current
// status and whether a retry successor is linked. The lock is held until the
// transaction ends, so the status check and the write that follows cannot be
// separated by another writer's CAS.
func lockExecutionRow(ctx context.Context, tx pgx.Tx, executionID string) (status string, retryLinked bool, err error) {
	if executionID == "" {
		return "", false, fmt.Errorf("guarded execution save: empty execution id")
	}
	var retryID *string
	err = tx.QueryRow(ctx,
		`SELECT status, retry_execution_id::text FROM purchase_executions WHERE execution_id = $1 FOR UPDATE`,
		executionID).Scan(&status, &retryID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, fmt.Errorf("%w: execution %s", ErrNotFound, executionID)
	}
	if err != nil {
		return "", false, fmt.Errorf("lock execution %s: %w", executionID, err)
	}
	return status, retryID != nil, nil
}

// SaveExecutionIfStatus is SavePurchaseExecution that only writes when the
// stored row is still in one of the expected statuses. A stale in-memory copy
// can therefore never rewrite a status another writer moved on (issue #829).
// A missing row returns ErrNotFound and a status mismatch returns
// ErrExecutionNotInExpectedStatus; neither writes anything.
func (s *PostgresStore) SaveExecutionIfStatus(ctx context.Context, execution *PurchaseExecution, expected []string) error {
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		return s.SaveExecutionIfStatusTx(ctx, tx, execution, expected)
	})
}

// SaveExecutionIfStatusTx is the tx-accepting variant of SaveExecutionIfStatus,
// for callers that bundle the save with other writes (for example releasing
// suppressions). It reuses SavePurchaseExecutionTx for the write, so the column
// list and the no_submission and INSERT-only rules exist in one place.
func (s *PostgresStore) SaveExecutionIfStatusTx(ctx context.Context, tx pgx.Tx, execution *PurchaseExecution, expected []string) error {
	status, _, err := lockExecutionRow(ctx, tx, execution.ExecutionID)
	if err != nil {
		return err
	}
	if !slices.Contains(expected, status) {
		return fmt.Errorf("%w: execution %s is %q, expected one of %v", ErrExecutionNotInExpectedStatus, execution.ExecutionID, status, expected)
	}
	return s.SavePurchaseExecutionTx(ctx, tx, execution)
}

// RecoverReapedExecutionTx persists a late successful outcome over a row the
// reaper already failed: it writes execution (whose Status must be completed or
// partially_completed) only if the stored row is still failed with no retry
// successor linked. A different status returns ErrExecutionNotInExpectedStatus;
// a linked successor returns ErrExecutionRetryLinked and writes nothing.
func (s *PostgresStore) RecoverReapedExecutionTx(ctx context.Context, tx pgx.Tx, execution *PurchaseExecution) error {
	if execution.Status != StatusCompleted && execution.Status != StatusPartiallyCompleted {
		return fmt.Errorf("recover reaped execution %s: status %q is not completed or partially_completed", execution.ExecutionID, execution.Status)
	}
	status, retryLinked, err := lockExecutionRow(ctx, tx, execution.ExecutionID)
	if err != nil {
		return err
	}
	if status != StatusFailed {
		return fmt.Errorf("%w: execution %s is %q, expected %q", ErrExecutionNotInExpectedStatus, execution.ExecutionID, status, StatusFailed)
	}
	if retryLinked {
		return fmt.Errorf("%w: execution %s", ErrExecutionRetryLinked, execution.ExecutionID)
	}
	return s.SavePurchaseExecutionTx(ctx, tx, execution)
}

// RecoverReapedExecution is RecoverReapedExecutionTx in its own transaction.
func (s *PostgresStore) RecoverReapedExecution(ctx context.Context, execution *PurchaseExecution) error {
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		return s.RecoverReapedExecutionTx(ctx, tx, execution)
	})
}
