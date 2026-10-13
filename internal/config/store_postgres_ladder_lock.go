package config

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
)

// ladderConfigLockPrefix namespaces the per-config ladder advisory lock. It is
// a distinct text input to the same hashtextextended(text, 0) bigint key space
// as planStepProviderLockPrefix and the purchase-submit lock; a 64-bit hash
// collision could only cause a spurious wait, never a correctness break.
const ladderConfigLockPrefix = "cudly:ladder-config:"

// ladderLockTimeout bounds only the advisory-lock wait so a stuck holder fails
// loudly instead of queueing every later run. Package var so a test can shrink it.
var ladderLockTimeout = 30 * time.Second

// LadderPlanBuilder computes a run and its new tranches from the config's
// in-flight commitment. It runs while the per-config lock is held inside the
// persist transaction: it must be pure compute (no provider or network I/O).
type LadderPlanBuilder func(inFlightUSDHr float64) (*LadderRunDB, []LadderTrancheDB, error)

// ErrLadderLockTimeout means the per-config ladder lock was not acquired within
// ladderLockTimeout (SQLSTATE 55P03). The operation did nothing; retry later.
var ErrLadderLockTimeout = errors.New("ladder config lock timed out")

func ladderConfigLockKey(configID string) string { return ladderConfigLockPrefix + configID }

// withLadderConfigLock runs fn in a READ COMMITTED transaction that holds the
// config's ladder lock. It is the only way planning (PlanAndSaveLadderRun) and
// claiming (ClaimDueLadderTranches) serialize on one config.
//
// Lock order: this lock is taken FIRST. Whatever fn takes afterwards (provider
// plan-step locks, LockPurchasePlanTx, row locks) comes after it; code holding
// an execution or tranche row lock must never take this lock.
//
// Isolation is set explicitly: under REPEATABLE READ the snapshot is fixed at
// the first statement, before the wait, so the in-flight read would be stale.
func (s *PostgresStore) withLadderConfigLock(ctx context.Context, configID string, fn func(tx pgx.Tx) error) error {
	if configID == "" {
		return errors.New("ladder config lock: empty config id")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		// ErrTxClosed after Commit is expected; the deferred call also covers a panic in fn.
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			log.Printf("ladder config lock %s: rollback: %v", configID, rbErr)
		}
	}()

	if err := acquireLadderConfigLock(ctx, tx, configID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}
	return nil
}

// acquireLadderConfigLock must be the first thing run in tx: the isolation
// level can only be set before any query.
func acquireLadderConfigLock(ctx context.Context, tx pgx.Tx, configID string) error {
	if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
		return fmt.Errorf("ladder config lock %s: set isolation: %w", configID, err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = %d", ladderLockTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("ladder config lock %s: set lock_timeout: %w", configID, err)
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", ladderConfigLockKey(configID)); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
			return fmt.Errorf("ladder config lock %s: %w: %w", configID, ErrLadderLockTimeout, err)
		}
		return fmt.Errorf("ladder config lock %s: %w", configID, err)
	}
	// The timeout was for the advisory wait only; do not leak it into fn's statements.
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout TO DEFAULT"); err != nil {
		return fmt.Errorf("ladder config lock %s: reset lock_timeout: %w", configID, err)
	}
	return nil
}

// ladderSettleSkewMargin widens the settled-tranche window backwards because
// settled_at is stamped with the DB clock while asOf comes from the app clock.
// A skew of up to this size cannot make a tranche that settled after the
// caller's existing-commitment read fall outside the window; the cost is a
// bounded double count (a safe under-buy) that heals on the next run. It must
// stay far below the shortest cadence (cadenceDailyMin, 20h), so consecutive
// runs' windows never overlap.
const ladderSettleSkewMargin = time.Minute

// ladderSettleAnchor is the anchor every caller passes to settledLadderCommit
// for an existing-commitment read taken at or after asOf.
func ladderSettleAnchor(asOf time.Time) time.Time { return asOf.Add(-ladderSettleSkewMargin) }

// PlanAndSaveLadderRun reads the config's in-flight commitment, lets build turn
// it into a run plus tranches, and persists both, all in ONE transaction under
// the config's ladder lock. Two overlapping runs for one config therefore
// cannot both plan the same gap: the second waits, then reads the first's
// committed tranches as in-flight. build must be pure compute. The run's
// ConfigID must equal configID and its StartedAt must equal asOf, which bounds
// both the netting read and the overdue sweep.
//
// asOf MUST be captured before the caller reads the existing commitment (E,
// GetLayerStates), which happens outside this transaction. Tranches that
// completed after asOf are added to the in-flight figure, so one completing
// between the E read and the locked read is netted (counted twice if E already
// has it: a safe under-buy, never a miss). This relies on a tranche being
// marked completed only after its commitment is visible through the same source
// as E, and on visibility never reverting; it is safe only once the visibility
// adapter (executor PR5a) provides that.
func (s *PostgresStore) PlanAndSaveLadderRun(ctx context.Context, configID string, asOf time.Time, build LadderPlanBuilder) (*LadderRunDB, error) {
	var result LadderRunDB
	err := s.withLadderConfigLock(ctx, configID, func(tx pgx.Tx) error {
		inFlight, err := lockedLadderCommit(ctx, tx, configID, asOf)
		if err != nil {
			return err
		}
		run, tranches, err := build(inFlight)
		if err != nil {
			return err
		}
		if run == nil {
			return errors.New("ladder plan builder returned a nil run")
		}
		if run.ConfigID == nil || *run.ConfigID != configID {
			return fmt.Errorf("ladder run config_id does not match locked config %s", configID)
		}
		// The netting read above and the overdue sweep in the insert must use
		// the same grace cutoff, or a row could be counted and canceled (or neither).
		if !run.StartedAt.Equal(asOf) {
			return fmt.Errorf("ladder run started_at %s does not match asOf %s", run.StartedAt, asOf)
		}
		if run.ID == "" {
			run.ID = uuid.New().String()
		}
		saved, err := insertLadderRunWithTranchesTx(ctx, tx, run, tranches)
		if err != nil {
			return err
		}
		result = *saved
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// lockedLadderCommit is the figure PlanAndSaveLadderRun hands to build:
// in-flight plus tranches settled after the skew-widened anchor.
//
// Order is load-bearing: in-flight FIRST, settled SECOND. Each statement takes
// a fresh READ COMMITTED snapshot, so a tranche fired at the first and
// completed before the second is counted by both (safe); the reverse order
// could miss one completing between the two.
func lockedLadderCommit(ctx context.Context, q queryRower, configID string, asOf time.Time) (float64, error) {
	inFlight, err := inFlightLadderCommit(ctx, q, configID, asOf)
	if err != nil {
		return 0, err
	}
	settled, err := settledLadderCommit(ctx, q, configID, ladderSettleAnchor(asOf))
	if err != nil {
		return 0, err
	}
	return inFlight + settled, nil
}

// queryRower is satisfied by pgx.Tx and the pool connection.
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// inFlightLadderCommit sums the config's in-flight commitment; see
// GetInFlightLadderCommitUSDHr for the row classes counted and excluded.
func inFlightLadderCommit(ctx context.Context, q queryRower, configID string, asOf time.Time) (float64, error) {
	var total float64
	err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_usd_hr), 0)
		FROM ladder_tranches
		WHERE config_id = $1
		  AND (status = $2 OR (status = $3 AND scheduled_date > $4))
	`, configID, string(ladder.TrancheStatusFired), string(ladder.TrancheStatusScheduled), ladderGraceCutoff(asOf)).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("in-flight ladder commit config_id=%s: %w", configID, err)
	}
	return total, nil
}
