package config

import (
	"context"
	"fmt"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/jackc/pgx/v5"
)

// LadderFireGrace is how late a scheduled ladder tranche may still be fired.
// It is the single definition behind both sides of the boundary: in-flight
// netting counts a scheduled tranche while it is still fireable, and the
// overdue sweep cancels it once it is not. Keeping both on ladderGraceCutoff
// guarantees every scheduled row is counted or canceled, never neither
// (neither would let the next run re-plan an amount the executor can still
// buy, and both would be bought) and never both.
const LadderFireGrace = 24 * time.Hour

// ladderGraceCutoff returns the instant at or before which a scheduled
// tranche is expired: scheduled_date <= cutoff is canceled, and
// scheduled_date > cutoff is still netted and claimable.
func ladderGraceCutoff(asOf time.Time) time.Time {
	return asOf.Add(-LadderFireGrace)
}

// ladderTrancheEdges lists the legal tranche status transitions. completed,
// failed and canceled are terminal. A fired tranche whose purchase outcome is
// unknown stays fired (and keeps netting) until it is resolved to completed or
// failed.
var ladderTrancheEdges = map[ladder.TrancheStatus][]ladder.TrancheStatus{
	ladder.TrancheStatusScheduled: {ladder.TrancheStatusFired, ladder.TrancheStatusCancelled},
	ladder.TrancheStatusFired:     {ladder.TrancheStatusCompleted, ladder.TrancheStatusFailed},
}

func validateLadderTrancheEdge(from, to ladder.TrancheStatus) error {
	for _, allowed := range ladderTrancheEdges[from] {
		if allowed == to {
			return nil
		}
	}
	return fmt.Errorf("illegal ladder tranche transition %q -> %q", from, to)
}

// TransitionLadderTranche moves one tranche from `from` to `to` with a
// compare-and-set UPDATE. It returns false (and no error) when the row is
// missing or no longer in `from`, so a lost race is distinguishable from a DB
// failure. An illegal edge is an error. scheduled -> fired is not allowed here
// because a claim must create the execution in the same transaction; use
// ClaimDueLadderTranches. A scheduled tranche that already has an execution
// cannot be canceled. reason is stored in status_reason and is required for
// the failed and canceled outcomes, so a terminal tranche always says why.
func (s *PostgresStore) TransitionLadderTranche(ctx context.Context, id string, from, to ladder.TrancheStatus, reason string) (bool, error) {
	var moved bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		moved, txErr = s.TransitionLadderTrancheTx(ctx, tx, id, from, to, reason)
		return txErr
	})
	return moved, err
}

// TransitionLadderTrancheTx is TransitionLadderTranche inside the caller's
// transaction, for callers that must re-check other rows under lock first.
func (s *PostgresStore) TransitionLadderTrancheTx(ctx context.Context, tx pgx.Tx, id string, from, to ladder.TrancheStatus, reason string) (bool, error) {
	if err := validateLadderTrancheEdge(from, to); err != nil {
		return false, err
	}
	if from == ladder.TrancheStatusScheduled && to == ladder.TrancheStatusFired {
		return false, fmt.Errorf("ladder tranche %s: scheduled -> fired must go through ClaimDueLadderTranches", id)
	}
	if reason == "" && (to == ladder.TrancheStatusFailed || to == ladder.TrancheStatusCancelled) {
		return false, fmt.Errorf("ladder tranche %s: a %s transition needs a status reason", id, to)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE ladder_tranches
		SET status = $3, status_reason = NULLIF($4, ''),
		    settled_at = CASE WHEN $3 = 'completed' THEN clock_timestamp() ELSE settled_at END
		WHERE id = $1
		  AND status = $2
		  AND ($2 <> 'scheduled' OR execution_id IS NULL)
	`, id, string(from), string(to), reason)
	if err != nil {
		return false, fmt.Errorf("failed to transition ladder_tranche id=%s %s -> %s: %w", id, from, to, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ClaimDueLadderTranches atomically claims up to limit tranches of configID
// that are due (scheduled_date <= asOf) but not expired (scheduled_date >
// asOf - LadderFireGrace). For each claimed row, in the same transaction,
// create runs and must return the id of the purchase_executions row it
// created; the tranche is then set to fired with that execution_id. If create
// returns an error the whole transaction rolls back and nothing is claimed, so
// a claim and its execution row can never disagree after a crash. Rows locked
// by a concurrent claim are skipped (FOR UPDATE SKIP LOCKED), so exactly one
// caller wins each tranche. The whole claim runs under the config's ladder
// lock (taken first), including the create callback: with a caller asOf earlier
// than the planner's, a claim could otherwise fire a row the planner's netting
// read had already excluded as expired, and the row would be bought AND
// re-planned. A callback slower than ladderLockTimeout makes planners fail loudly.
func (s *PostgresStore) ClaimDueLadderTranches(
	ctx context.Context,
	configID string,
	asOf time.Time,
	limit int,
	create func(ctx context.Context, tx pgx.Tx, tranche LadderTrancheDB) (executionID string, err error),
) ([]LadderTrancheDB, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("ClaimDueLadderTranches limit must be positive, got %d", limit)
	}
	var claimed []LadderTrancheDB
	err := s.withLadderConfigLock(ctx, configID, func(tx pgx.Tx) error {
		due, err := selectDueLadderTranchesTx(ctx, tx, configID, asOf, limit)
		if err != nil {
			return err
		}
		for i := range due {
			execID, err := create(ctx, tx, due[i])
			if err != nil {
				return fmt.Errorf("create execution for ladder_tranche id=%s: %w", due[i].ID, err)
			}
			if execID == "" {
				return fmt.Errorf("create execution for ladder_tranche id=%s returned an empty execution id", due[i].ID)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE ladder_tranches SET status = $2, execution_id = $3
				WHERE id = $1 AND status = 'scheduled' AND execution_id IS NULL
			`, due[i].ID, string(ladder.TrancheStatusFired), execID); err != nil {
				return fmt.Errorf("failed to claim ladder_tranche id=%s: %w", due[i].ID, err)
			}
			due[i].Status = ladder.TrancheStatusFired
			due[i].ExecutionID = &execID
		}
		claimed = due
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

func selectDueLadderTranchesTx(ctx context.Context, tx pgx.Tx, configID string, asOf time.Time, limit int) ([]LadderTrancheDB, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, config_id, run_id, layer_type, amount_usd_hr, term,
		       payment_option, scheduled_date, status, settled_at, created_at, amount_basis, commitment_rate
		FROM ladder_tranches
		WHERE config_id = $1
		  AND status = 'scheduled'
		  AND execution_id IS NULL
		  AND scheduled_date <= $2
		  AND scheduled_date > $3
		ORDER BY scheduled_date, id
		LIMIT $4
		FOR UPDATE SKIP LOCKED
	`, configID, asOf, ladderGraceCutoff(asOf), limit)
	if err != nil {
		return nil, fmt.Errorf("failed to select due ladder_tranches config_id=%s: %w", configID, err)
	}
	defer rows.Close()
	var out []LadderTrancheDB
	for rows.Next() {
		var tr LadderTrancheDB
		if err := rows.Scan(&tr.ID, &tr.ConfigID, &tr.RunID, &tr.LayerType, &tr.AmountUSDHr, &tr.Term,
			&tr.PaymentOption, &tr.ScheduledDate, &tr.Status, &tr.SettledAt, &tr.CreatedAt, &tr.AmountBasis, &tr.CommitmentRate); err != nil {
			return nil, fmt.Errorf("failed to scan due ladder_tranche: %w", err)
		}
		if _, err := trancheCommitmentRate(&tr); err != nil {
			return nil, fmt.Errorf("invalid due ladder_tranche %s: %w", tr.ID, err)
		}
		out = append(out, tr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read due ladder_tranches: %w", err)
	}
	return out, nil
}
