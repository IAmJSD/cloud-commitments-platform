package config

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
)

const ladderMicros = 1_000_000

// ladderMaxUSDHr keeps micro-dollar sums far below int64 overflow (about 9.2e12 USD/hr).
const ladderMaxUSDHr = 1e12

// toLadderMicros converts a USD/hr amount (at most six fractional digits) to
// integer micro-dollars, so the ceiling comparison is exact.
func toLadderMicros(v float64) int64 { return int64(math.Round(v * ladderMicros)) }

// checkLadderConfigCeiling refuses a RAISE that would push the config's
// commitment past what its latest non-canceled run planned to leave room for.
// Decreases, re-date-only edits and cancels always pass.
//
// Committed = in-flight (fired + scheduled, all runs, with the amended row at
// its new amount) + tranches completed after that run started, less the
// ladderSettleSkewMargin for clock skew (their
// commitment may not be in the run's existing_usd_hr snapshot yet). Every term
// is netted per row in on-demand-equivalent dollars, the unit of the ceiling;
// the amended row's old and new amounts convert by its own basis and rate.
// Ceiling C = min(target, baseline * current coverage%) - existing, from the
// run's snapshot (existing is gross, so C errs low) and the config's current
// coverage. A run without a full snapshot refuses a raise. Must run inside
// withLadderConfigLock so the sums cannot move under it.
func checkLadderConfigCeiling(ctx context.Context, tx pgx.Tx, configID string, amended lockedLadderTranche, newAmount string) error {
	var raise bool
	if err := tx.QueryRow(ctx, `SELECT $1::numeric>$2::numeric`, newAmount, amended.amount).Scan(&raise); err != nil {
		return err
	}
	if !raise {
		return nil
	}
	snap, err := loadLadderCeiling(ctx, tx, configID)
	if err != nil {
		return err
	}
	// Same sweep as the planner: a scheduled row past the grace window is neither
	// counted nor claimable, so cancel it here or a later claim with an older asOf
	// could still fire it on top of the raise.
	now := time.Now()
	err = cancelOverdueLadderTranchesTx(ctx, tx, &configID, now)
	if err != nil {
		return err
	}
	committed, err := ladderCommittedWithAmend(ctx, tx, configID, now, ladderSettleAnchor(snap.anchor), amended, newAmount)
	if err != nil {
		return err
	}
	if snap.ceiling > ladderMaxUSDHr {
		return fmt.Errorf("%w: amount beyond %.0f USD/hr", ErrLadderAmendInvalid, ladderMaxUSDHr)
	}
	if committed.Cmp(big.NewRat(toLadderMicros(snap.ceiling), ladderMicros)) > 0 {
		return ErrLadderAmendInvalid
	}
	return nil
}

// ladderCommittedWithAmend is the on-demand-equivalent committed total with the
// amended row swapped from its old to its new amount, each converted by that
// row's own basis and rate. The rows still hold the old amount.
func ladderCommittedWithAmend(ctx context.Context, tx pgx.Tx, configID string, now, anchor time.Time, amended lockedLadderTranche, newAmount string) (*big.Rat, error) {
	inFlightRows, err := inFlightLadderRows(ctx, tx, true, configID, now)
	if err != nil {
		return nil, err
	}
	settledRows, err := settledLadderRows(ctx, tx, true, configID, anchor)
	if err != nil {
		return nil, err
	}
	committed, err := sumLadderOnDemand(inFlightRows, settledRows)
	if err != nil {
		return nil, err
	}
	tr := assumeOnDemandIfUnknown(&LadderTrancheDB{AmountBasis: amended.basis, CommitmentRate: amended.rate})
	oldOD, err := nettingOnDemand(amended.amount, tr)
	if err != nil {
		return nil, err
	}
	newOD, err := nettingOnDemand(newAmount, tr)
	if err != nil {
		return nil, err
	}
	maxOD := new(big.Rat).SetInt64(ladderMaxUSDHr)
	for _, v := range []*big.Rat{committed, oldOD, newOD} {
		if v.Cmp(maxOD) > 0 {
			return nil, fmt.Errorf("%w: amount beyond %.0f USD/hr", ErrLadderAmendInvalid, ladderMaxUSDHr)
		}
	}
	return committed.Sub(committed, oldOD).Add(committed, newOD), nil
}

type ladderCeilingSnapshot struct {
	anchor  time.Time
	ceiling float64
}

// loadLadderCeiling reads the latest non-canceled run's snapshot joined with the
// config's current coverage. It refuses a run without a full snapshot.
func loadLadderCeiling(ctx context.Context, tx pgx.Tx, configID string) (ladderCeilingSnapshot, error) {
	var snap ladderCeilingSnapshot
	var hasSnapshot bool
	err := tx.QueryRow(ctx, `SELECT r.started_at,
 r.target_usd_hr IS NOT NULL AND r.existing_usd_hr IS NOT NULL AND r.baseline_usd_hr IS NOT NULL,
 COALESCE(trunc(LEAST(r.target_usd_hr,r.baseline_usd_hr*c.target_coverage/100)-r.existing_usd_hr,6),0)::float8
 FROM ladder_runs r JOIN ladder_configs c ON c.id=r.config_id
 WHERE r.config_id=$1 AND r.status<>'cancelled' ORDER BY r.started_at DESC,r.created_at DESC,r.id DESC LIMIT 1`,
		configID).Scan(&snap.anchor, &hasSnapshot, &snap.ceiling)
	if errors.Is(err, pgx.ErrNoRows) {
		return snap, fmt.Errorf("%w: config has no non-canceled run", ErrLadderAmendInvalid)
	}
	if err != nil {
		return snap, err
	}
	if !hasSnapshot {
		return snap, fmt.Errorf("%w: latest run has no target snapshot to bound a raise", ErrLadderAmendInvalid)
	}
	return snap, nil
}
