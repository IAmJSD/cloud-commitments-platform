package config

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/jackc/pgx/v5"
)

// LadderChainMaxDepth caps how many executions the reconciler follows along a
// retry chain (head = the execution the tranche was claimed with). A longer
// chain is returned truncated, which the reconciler treats as unresolved.
const LadderChainMaxDepth = 20

// ErrLadderTrancheNotFired means the tranche row is gone or no longer fired
// when the reconciler locked it: another writer won, nothing to do.
var ErrLadderTrancheNotFired = errors.New("ladder tranche is not fired")

// LadderChainNode is the part of one purchase_executions row the reconciler
// reads. NoSubmission is only meaningful while Status is failed.
type LadderChainNode struct {
	ExecutionID      string
	Status           string
	NoSubmission     bool
	RetryExecutionID *string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// ListFiredLadderTranches returns fired tranches that have an execution,
// oldest first, for the reconciler to examine one by one.
func (s *PostgresStore) ListFiredLadderTranches(ctx context.Context, limit int) ([]LadderTrancheDB, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("ListFiredLadderTranches limit must be positive, got %d", limit)
	}
	rows, err := s.db.Query(ctx, `
		SELECT id, config_id, run_id, execution_id, layer_type, amount_usd_hr, term,
		       payment_option, scheduled_date, status, settled_at, created_at
		FROM ladder_tranches
		WHERE status = 'fired' AND execution_id IS NOT NULL
		ORDER BY scheduled_date, id
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list fired ladder_tranches: %w", err)
	}
	defer rows.Close()
	var out []LadderTrancheDB
	for rows.Next() {
		var tr LadderTrancheDB
		if err := rows.Scan(&tr.ID, &tr.ConfigID, &tr.RunID, &tr.ExecutionID, &tr.LayerType, &tr.AmountUSDHr, &tr.Term,
			&tr.PaymentOption, &tr.ScheduledDate, &tr.Status, &tr.SettledAt, &tr.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan fired ladder_tranche: %w", err)
		}
		out = append(out, tr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read fired ladder_tranches: %w", err)
	}
	return out, nil
}

// LockLadderTrancheChainTx locks a fired tranche, follows retry_execution_id
// from its execution to the tail (at most LadderChainMaxDepth nodes, which also
// stops a retry cycle), then locks the tail execution row and re-reads it.
// LinkRetryExecutionAtomic updates that same row, so once this returns a retry
// cannot link a successor until the caller's transaction ends. The chain is
// returned head first. A chain that was cut short by the depth cap (a cycle hits it) or
// a tail that changed between the walk and the lock ends with a node whose
// RetryExecutionID is set; the caller must treat that as unresolved.
func (s *PostgresStore) LockLadderTrancheChainTx(ctx context.Context, tx pgx.Tx, trancheID string) ([]LadderChainNode, error) {
	var headID *string
	err := tx.QueryRow(ctx,
		`SELECT execution_id FROM ladder_tranches WHERE id = $1 AND status = 'fired' FOR UPDATE`, trancheID).Scan(&headID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && headID == nil) {
		return nil, fmt.Errorf("%w: %s", ErrLadderTrancheNotFired, trancheID)
	}
	if err != nil {
		return nil, fmt.Errorf("lock ladder_tranche %s: %w", trancheID, err)
	}
	var chain []LadderChainNode
	next := headID
	for next != nil && len(chain) < LadderChainMaxDepth {
		node, nodeErr := readChainNode(ctx, tx, *next, false)
		if nodeErr != nil {
			return nil, nodeErr
		}
		chain = append(chain, node)
		next = node.RetryExecutionID
	}
	tail, err := readChainNode(ctx, tx, chain[len(chain)-1].ExecutionID, true)
	if err != nil {
		return nil, err
	}
	chain[len(chain)-1] = tail
	return chain, nil
}

func readChainNode(ctx context.Context, tx pgx.Tx, executionID string, forUpdate bool) (LadderChainNode, error) {
	q := `SELECT execution_id::text, status, no_submission, retry_execution_id::text, created_at, updated_at
	      FROM purchase_executions WHERE execution_id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	var n LadderChainNode
	err := tx.QueryRow(ctx, q, executionID).Scan(&n.ExecutionID, &n.Status, &n.NoSubmission, &n.RetryExecutionID, &n.CreatedAt, &n.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return n, fmt.Errorf("%w: execution %s", ErrNotFound, executionID)
	}
	if err != nil {
		return n, fmt.Errorf("read execution %s: %w", executionID, err)
	}
	return n, nil
}

// LockLadderTrancheForRetryTx locks the ladder tranche whose id equals a
// purchase execution's idempotency key, so Retry can decide inside its own
// transaction before it inserts a successor. The key is compared as text:
// non-ladder keys (legacy execution ids, "<root>:<account>" multi-account
// keys, content fingerprints) are not UUIDs and simply match nothing, which
// is "not a ladder execution" (found=false), not an error. Lock order is
// tranche first, then execution rows, the same as LockLadderTrancheChainTx.
func (s *PostgresStore) LockLadderTrancheForRetryTx(ctx context.Context, tx pgx.Tx, idempotencyKey string) (status ladder.TrancheStatus, found bool, err error) {
	if idempotencyKey == "" {
		return "", false, nil
	}
	var raw string
	err = tx.QueryRow(ctx, `SELECT status FROM ladder_tranches WHERE id::text = $1 FOR UPDATE`, idempotencyKey).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("lock ladder_tranche for retry key %q: %w", idempotencyKey, err)
	}
	return ladder.TrancheStatus(raw), true, nil
}

// LadderExecutionGate is what the executor re-reads, right before a provider
// call, about the ladder tranche an execution belongs to. Each field is a
// pointer because the foreign keys are ON DELETE SET NULL: a nil field means
// the row is gone, which the caller must treat as "refuse", not as a default.
type LadderExecutionGate struct {
	ConfigEnabled *bool
	ConfigMode    *string
	RunStatus     *string
}

// GetLadderExecutionGate finds the ladder tranche whose id equals an
// execution's idempotency key and returns its config switches and run status.
// The key is compared as text, so non-ladder keys (legacy execution ids,
// "<root>:<account>" multi-account keys, fingerprints) match nothing and
// return (nil, nil): "not a ladder execution". Only a real database error is
// an error, and the caller must fail closed on it.
func (s *PostgresStore) GetLadderExecutionGate(ctx context.Context, idempotencyKey string) (*LadderExecutionGate, error) {
	if idempotencyKey == "" {
		return nil, nil
	}
	var g LadderExecutionGate
	err := s.db.QueryRow(ctx, `
		SELECT c.enabled, c.mode, r.status
		  FROM ladder_tranches t
		  LEFT JOIN ladder_configs c ON c.id = t.config_id
		  LEFT JOIN ladder_runs r ON r.id = t.run_id
		 WHERE t.id::text = $1`, idempotencyKey).Scan(&g.ConfigEnabled, &g.ConfigMode, &g.RunStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read ladder execution gate for key %q: %w", idempotencyKey, err)
	}
	return &g, nil
}

// LadderTrancheSettled reports whether a tranche status can no longer lead to
// a purchase through its existing execution: anything but scheduled or fired.
// Unknown statuses count as settled, so Retry fails closed.
func LadderTrancheSettled(status ladder.TrancheStatus) bool {
	return status != ladder.TrancheStatusScheduled && status != ladder.TrancheStatusFired
}
