package config

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/jackc/pgx/v5"
)

// Outcomes of an operator resolve that the store distinguishes. The handler
// maps them to 404, 400 and 409.
var (
	ErrLadderResolveNotFound = errors.New("ladder tranche not found")
	ErrLadderResolveInvalid  = errors.New("invalid ladder resolve request")
	ErrLadderResolveConflict = errors.New("ladder tranche cannot be resolved")
)

// ladderStatusReasonMax bounds status_reason; the column has no CHECK.
const ladderStatusReasonMax = 1000

// LadderCloudCheck is the operator's statement of where they looked for the
// purchase before declaring an unproven tranche failed.
type LadderCloudCheck struct {
	AccountID string
	Region    string
	Service   string
	CheckedAt time.Time
}

// LadderResolveFailed asks to move a fired tranche to failed. StatusReason is
// the complete string to store, built by the caller so that a repeat of the
// same request compares equal as a whole. Check is set when the caller
// acknowledges an unknown outcome.
type LadderResolveFailed struct {
	TrancheID    string
	AccountID    string
	StatusReason string
	Acknowledge  bool
	Check        *LadderCloudCheck
	Now          time.Time
	// StaleAfter is how long the chain must have been unchanged before an
	// unknown outcome may be declared failed (purchase.LadderFiredStaleAfter).
	StaleAfter time.Duration
}

// LadderResolveResult is the tranche after the call. Replayed is true when an
// identical earlier request had already failed it and nothing was written.
type LadderResolveResult struct {
	ID           string
	Status       string
	StatusReason string
	Replayed     bool
}

// ladderResolvableNodeStatus is the allowlist of execution statuses a chain
// may contain; anything else (pending, approved, running, a future status) can
// still move money, so it blocks the resolve.
func ladderResolvableNodeStatus(status string) bool {
	switch status {
	case StatusFailed, StatusCanceled, StatusExpired, StatusCompleted, StatusPartiallyCompleted:
		return true
	}
	return false
}

// ladderNodeProvenUnsent reports whether nothing in the node can have reached
// the provider.
func ladderNodeProvenUnsent(n LadderChainNode) bool {
	switch n.Status {
	case StatusCanceled, StatusExpired:
		return true
	case StatusFailed:
		return n.NoSubmission
	}
	return false
}

// ResolveLadderTrancheFailed moves a fired tranche to failed on an operator's
// word, releasing its netting. It locks the tranche, then the retry-chain
// tail, in the same order as the reconciler and Retry. It refuses while any
// execution of the chain may still run or has bought something, and, when a
// node's outcome is unknown (for example reaped), unless the operator
// acknowledged that and named where they checked.
func (s *PostgresStore) ResolveLadderTrancheFailed(ctx context.Context, req LadderResolveFailed) (*LadderResolveResult, error) {
	if len(req.StatusReason) > ladderStatusReasonMax {
		return nil, fmt.Errorf("%w: status reason is longer than %d", ErrLadderResolveInvalid, ladderStatusReasonMax)
	}
	res := &LadderResolveResult{ID: req.TrancheID, Status: string(ladder.TrancheStatusFailed), StatusReason: req.StatusReason}
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		chain, err := s.LockLadderTrancheChainTx(ctx, tx, req.TrancheID)
		if err != nil {
			return err
		}
		if checkErr := s.checkLadderResolveFailed(ctx, tx, req, chain); checkErr != nil {
			return checkErr
		}
		moved, err := s.TransitionLadderTrancheTx(ctx, tx, req.TrancheID, ladder.TrancheStatusFired, ladder.TrancheStatusFailed, req.StatusReason)
		if err != nil {
			return err
		}
		if !moved {
			return fmt.Errorf("%w: tranche changed while resolving", ErrLadderResolveConflict)
		}
		return nil
	})
	if errors.Is(err, ErrLadderTrancheNotFired) {
		return s.replayLadderResolve(ctx, req, res)
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

// replayLadderResolve answers a request that found the tranche no longer
// fired: 200 only if it is failed with exactly the status reason this request
// would have written, else a conflict naming the current state.
func (s *PostgresStore) replayLadderResolve(ctx context.Context, req LadderResolveFailed, res *LadderResolveResult) (*LadderResolveResult, error) {
	var status string
	var reason *string
	err := s.db.QueryRow(ctx, `SELECT status, status_reason FROM ladder_tranches WHERE id = $1`, req.TrancheID).Scan(&status, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrLadderResolveNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read ladder tranche %s: %w", req.TrancheID, err)
	}
	if status == string(ladder.TrancheStatusFailed) && reason != nil && *reason == req.StatusReason {
		res.Replayed = true
		return res, nil
	}
	return nil, fmt.Errorf("%w: tranche is %s", ErrLadderResolveConflict, status)
}

func (s *PostgresStore) checkLadderResolveFailed(ctx context.Context, tx pgx.Tx, req LadderResolveFailed, chain []LadderChainNode) error {
	var layer, account, external string
	err := tx.QueryRow(ctx, `SELECT t.layer_type, c.cloud_account_id::text, COALESCE(a.external_id, '')
		FROM ladder_tranches t
		JOIN ladder_configs c ON c.id = t.config_id
		JOIN cloud_accounts a ON a.id = c.cloud_account_id
		WHERE t.id = $1`, req.TrancheID).Scan(&layer, &account, &external)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && account != req.AccountID) {
		return ErrLadderResolveNotFound
	}
	if err != nil {
		return fmt.Errorf("read ladder tranche scope %s: %w", req.TrancheID, err)
	}
	if chain[len(chain)-1].RetryExecutionID != nil {
		return fmt.Errorf("%w: retry chain is longer than %d executions or changed", ErrLadderResolveConflict, LadderChainMaxDepth)
	}
	unproven, lastChange, err := scanLadderResolveChain(chain)
	if err != nil {
		return err
	}
	if !unproven {
		return nil
	}
	return validateLadderUnverifiedFail(req, layer, []string{account, external}, lastChange)
}

// scanLadderResolveChain applies the allowlist and completed-node rules and
// reports whether any node's outcome is unknown and when the chain last changed.
func scanLadderResolveChain(chain []LadderChainNode) (unproven bool, lastChange time.Time, err error) {
	for _, n := range chain {
		if !ladderResolvableNodeStatus(n.Status) {
			return false, lastChange, fmt.Errorf("%w: execution %s is %s and may still run", ErrLadderResolveConflict, n.ExecutionID, n.Status)
		}
		if n.Status == StatusCompleted || n.Status == StatusPartiallyCompleted {
			return false, lastChange, fmt.Errorf("%w: execution %s is %s, so a purchase may exist; failing the tranche would re-buy it", ErrLadderResolveConflict, n.ExecutionID, n.Status)
		}
		unproven = unproven || !ladderNodeProvenUnsent(n)
		if n.UpdatedAt.After(lastChange) {
			lastChange = n.UpdatedAt
		}
	}
	return unproven, lastChange, nil
}

func validateLadderUnverifiedFail(req LadderResolveFailed, layer string, accounts []string, lastChange time.Time) error {
	if !req.Acknowledge || req.Check == nil {
		return fmt.Errorf("%w: an execution of this tranche has an unknown outcome; acknowledge_unknown_outcome and cloud_check are required", ErrLadderResolveConflict)
	}
	if req.StaleAfter <= 0 {
		return errors.New("ladder resolve needs a positive StaleAfter")
	}
	if req.Now.Sub(lastChange) <= req.StaleAfter {
		return fmt.Errorf("%w: the chain changed at %s; wait until it is %s old", ErrLadderResolveConflict, lastChange.UTC().Format(time.RFC3339), req.StaleAfter)
	}
	c := req.Check
	switch {
	case !slices.Contains(accounts, c.AccountID):
		return fmt.Errorf("%w: checked_account_id is not the tranche's account (id or provider account number)", ErrLadderResolveInvalid)
	case c.Service != layer:
		return fmt.Errorf("%w: checked_service must be %q", ErrLadderResolveInvalid, layer)
	case c.CheckedAt.After(req.Now):
		return fmt.Errorf("%w: checked_at is in the future", ErrLadderResolveInvalid)
	case !c.CheckedAt.After(lastChange):
		return fmt.Errorf("%w: checked_at must be after the last execution change at %s", ErrLadderResolveInvalid, lastChange.UTC().Format(time.RFC3339))
	}
	return nil
}
