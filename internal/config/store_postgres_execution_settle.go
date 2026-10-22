package config

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Outcomes of an operator settle that the store distinguishes. The handler
// maps them to 404, 400 and 409.
var (
	ErrExecutionSettleNotFound = errors.New("execution not found")
	ErrExecutionSettleInvalid  = errors.New("invalid execution settle request")
	ErrExecutionSettleConflict = errors.New("execution cannot be settled")
)

// executionSettleReasonMax bounds the stored record; the column has no CHECK.
const executionSettleReasonMax = 1000

// ExecutionSettleRequest asks to settle a failed plan execution whose outcome
// is unknown. Actor is the acting principal's id. Acknowledge must be true:
// the operator states they checked the cloud account and accept that the row
// will never be retried.
type ExecutionSettleRequest struct {
	ExecutionID string
	Actor       string
	Reason      string
	Acknowledge bool
}

// ExecutionSettlement is the stored settlement. Replayed is true when an
// identical earlier request had already settled the row and nothing was
// written.
type ExecutionSettlement struct {
	ExecutionID string
	Status      string
	SettledAt   time.Time
	SettledBy   string
	Settlement  string
	Replayed    bool
}

// settleTarget is the locked row SettleFailedExecution judges.
type settleTarget struct {
	status       string
	noSubmission bool
	retryID      *string
	planID       *string
	settledAt    *time.Time
	settledBy    *string
	settlement   *string
}

// SettleFailedExecution records an operator's audited settlement of a failed
// plan execution that has no no_submission proof, so it stops blocking the
// delete of its editor-managed plan (requireManagedDeleteTx). It never changes
// the status and never makes the row count as unbought: the ramp ledger and
// the retention trigger keep treating it as possibly bought. A settled row can
// no longer be retried (LinkRetryExecutionAtomic), because a retry of a row
// whose plan was deleted would buy with no plan.
//
// Locks, in the order the editor command and DeletePurchasePlan use: the plan
// row FOR SHARE, the execution FOR UPDATE, then its retry-chain tail FOR
// UPDATE. It refuses while the chain may still buy (a tail that is not
// terminal, a failed tail that can still be retried, or a chain longer than
// LadderChainMaxDepth).
func (s *PostgresStore) SettleFailedExecution(ctx context.Context, req ExecutionSettleRequest) (*ExecutionSettlement, error) {
	if err := validateSettleRequest(req); err != nil {
		return nil, err
	}
	var res *ExecutionSettlement
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		target, err := lockSettleTarget(ctx, tx, req.ExecutionID)
		if err != nil {
			return err
		}
		if target.settledAt != nil {
			res, err = replaySettlement(req, target)
			return err
		}
		if checkErr := checkSettleTarget(target); checkErr != nil {
			return checkErr
		}
		tail, err := lockSettleChainTail(ctx, tx, req.ExecutionID, target.retryID)
		if err != nil {
			return err
		}
		record := settlementRecord(req, tail)
		res = &ExecutionSettlement{ExecutionID: req.ExecutionID, Status: target.status, SettledBy: req.Actor, Settlement: record}
		return tx.QueryRow(ctx, `UPDATE purchase_executions
			SET outcome_settled_at = NOW(), outcome_settled_by = $2, outcome_settlement = $3
			WHERE execution_id = $1 AND outcome_settled_at IS NULL
			RETURNING outcome_settled_at`, req.ExecutionID, req.Actor, record).Scan(&res.SettledAt)
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func validateSettleRequest(req ExecutionSettleRequest) error {
	switch {
	case !req.Acknowledge:
		return fmt.Errorf("%w: the operator must acknowledge the unknown outcome", ErrExecutionSettleInvalid)
	case req.Actor == "":
		return fmt.Errorf("%w: settling needs an actor", ErrExecutionSettleInvalid)
	case req.Reason == "" || len(req.Reason) > executionSettleReasonMax:
		return fmt.Errorf("%w: reason must be 1 to %d bytes", ErrExecutionSettleInvalid, executionSettleReasonMax)
	}
	return nil
}

// lockSettleTarget takes the plan lock, then the execution lock, and re-reads
// the row under both. A row whose plan changed in between (deleted, so
// plan_id went NULL) is refused rather than judged against the wrong plan.
func lockSettleTarget(ctx context.Context, tx pgx.Tx, executionID string) (*settleTarget, error) {
	var planID *string
	err := tx.QueryRow(ctx, `SELECT plan_id::text FROM purchase_executions WHERE execution_id = $1`, executionID).Scan(&planID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrExecutionSettleNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read execution %s: %w", executionID, err)
	}
	if planID == nil {
		return nil, fmt.Errorf("%w: only an execution of a purchase plan can be settled", ErrExecutionSettleConflict)
	}
	if _, lockErr := tx.Exec(ctx, `SELECT 1 FROM purchase_plans WHERE id = $1 FOR SHARE`, *planID); lockErr != nil {
		return nil, fmt.Errorf("lock plan %s: %w", *planID, lockErr)
	}
	var t settleTarget
	err = tx.QueryRow(ctx, `SELECT status, no_submission, retry_execution_id::text, plan_id::text,
		outcome_settled_at, outcome_settled_by, outcome_settlement
		FROM purchase_executions WHERE execution_id = $1 FOR UPDATE`, executionID).Scan(
		&t.status, &t.noSubmission, &t.retryID, &t.planID, &t.settledAt, &t.settledBy, &t.settlement)
	if err != nil {
		return nil, fmt.Errorf("lock execution %s: %w", executionID, err)
	}
	if t.planID == nil || *t.planID != *planID {
		return nil, fmt.Errorf("%w: the execution's plan changed while settling", ErrExecutionSettleConflict)
	}
	return &t, nil
}

// checkSettleTarget allows only the rows that block a managed delete for an
// unknown outcome: failed without a no_submission proof.
func checkSettleTarget(t *settleTarget) error {
	if t.status != StatusFailed {
		return fmt.Errorf("%w: execution is %s; only a failed execution can be settled", ErrExecutionSettleConflict, t.status)
	}
	if t.noSubmission {
		return fmt.Errorf("%w: execution is proven never submitted and needs no settlement", ErrExecutionSettleConflict)
	}
	return nil
}

// settleChainTail is the last execution of the retry chain from the target.
type settleChainTail struct {
	id     string
	status string
}

// lockSettleChainTail follows retry_execution_id from the target to the tail
// and locks the tail. It refuses while the chain may still buy: a tail outside
// the terminal allowlist, a failed tail that can still be retried (no
// no_submission proof and not settled itself), or a chain cut short by the
// depth cap or changed between the walk and the lock.
func lockSettleChainTail(ctx context.Context, tx pgx.Tx, executionID string, next *string) (settleChainTail, error) {
	tail, err := walkSettleChain(ctx, tx, executionID, next)
	if err != nil || tail.id == executionID {
		return tail, err
	}
	var retryID *string
	var noSubmission, settled bool
	err = tx.QueryRow(ctx, `SELECT status, no_submission, retry_execution_id::text, outcome_settled_at IS NOT NULL
		FROM purchase_executions WHERE execution_id = $1 FOR UPDATE`, tail.id).Scan(&tail.status, &noSubmission, &retryID, &settled)
	if err != nil {
		return tail, fmt.Errorf("lock execution %s: %w", tail.id, err)
	}
	if retryID != nil {
		return tail, fmt.Errorf("%w: the retry chain changed while settling", ErrExecutionSettleConflict)
	}
	retryable := tail.status == StatusFailed && !noSubmission && !settled
	if !ladderResolvableNodeStatus(tail.status) || retryable {
		return tail, fmt.Errorf("%w: retry %s is %s and may still buy or be retried; let it finish or settle it first", ErrExecutionSettleConflict, tail.id, tail.status)
	}
	return tail, nil
}

// walkSettleChain reads the retry chain from the target, unlocked, and returns
// its last node; the target itself when it was never retried.
func walkSettleChain(ctx context.Context, tx pgx.Tx, executionID string, next *string) (settleChainTail, error) {
	tail := settleChainTail{id: executionID, status: StatusFailed}
	for depth := 0; next != nil; depth++ {
		if depth >= LadderChainMaxDepth {
			return tail, fmt.Errorf("%w: retry chain is longer than %d executions", ErrExecutionSettleConflict, LadderChainMaxDepth)
		}
		node, err := readChainNode(ctx, tx, *next, false)
		if err != nil {
			return tail, err
		}
		tail, next = settleChainTail{id: node.ExecutionID, status: node.Status}, node.RetryExecutionID
	}
	return tail, nil
}

// settlementRecord is the exact string stored in outcome_settlement. Fields are
// fixed and quoted, so a repeat of the same request compares equal and a
// different one cannot.
func settlementRecord(req ExecutionSettleRequest, tail settleChainTail) string {
	return fmt.Sprintf("operator:settled_unknown_outcome: ack=true chain_tail=%s tail_status=%s reason=%q",
		tail.id, tail.status, req.Reason)
}

// replaySettlement answers a request for a row that is already settled: the
// stored settlement when this actor sent the same reason, else a conflict.
func replaySettlement(req ExecutionSettleRequest, t *settleTarget) (*ExecutionSettlement, error) {
	if t.settledBy == nil || t.settlement == nil {
		return nil, fmt.Errorf("execution %s has an incomplete settlement", req.ExecutionID)
	}
	if *t.settledBy != req.Actor || !settlementHasReason(*t.settlement, req.Reason) {
		return nil, fmt.Errorf("%w: already settled by %s at %s", ErrExecutionSettleConflict, *t.settledBy, t.settledAt.UTC().Format(time.RFC3339))
	}
	return &ExecutionSettlement{ExecutionID: req.ExecutionID, Status: t.status, SettledAt: *t.settledAt, SettledBy: *t.settledBy, Settlement: *t.settlement, Replayed: true}, nil
}

func settlementHasReason(record, reason string) bool {
	suffix := fmt.Sprintf(" reason=%q", reason)
	return len(record) >= len(suffix) && record[len(record)-len(suffix):] == suffix
}
