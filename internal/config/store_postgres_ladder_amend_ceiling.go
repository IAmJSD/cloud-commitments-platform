package config

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

const ladderMicros = 1e6

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
// commitment may not be in the run's existing_usd_hr snapshot yet).
// Ceiling C = min(target, baseline * current coverage%) - existing, from the
// run's snapshot (existing is gross, so C errs low) and the config's current
// coverage. A run without a full snapshot refuses a raise. Must run inside
// withLadderConfigLock so the sums cannot move under it.
func checkLadderConfigCeiling(ctx context.Context, tx pgx.Tx, configID, oldAmount, newAmount string) error {
	var raise bool
	if err := tx.QueryRow(ctx, `SELECT $1::numeric>$2::numeric`, newAmount, oldAmount).Scan(&raise); err != nil {
		return err
	}
	if !raise {
		return nil
	}
	snap, err := loadLadderCeiling(ctx, tx, configID, oldAmount, newAmount)
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
	inFlight, err := inFlightLadderCommit(ctx, tx, configID, now)
	if err != nil {
		return err
	}
	settled, err := settledLadderCommit(ctx, tx, configID, ladderSettleAnchor(snap.anchor))
	if err != nil {
		return err
	}
	for _, v := range []float64{inFlight, settled, snap.ceiling, snap.oldAmount, snap.newAmount} {
		if v > ladderMaxUSDHr {
			return fmt.Errorf("%w: amount beyond %.0f USD/hr", ErrLadderAmendInvalid, ladderMaxUSDHr)
		}
	}
	committed := toLadderMicros(inFlight) - toLadderMicros(snap.oldAmount) + toLadderMicros(snap.newAmount) + toLadderMicros(settled)
	if committed > toLadderMicros(snap.ceiling) {
		return ErrLadderAmendInvalid
	}
	return nil
}

type ladderCeilingSnapshot struct {
	anchor                        time.Time
	ceiling, oldAmount, newAmount float64
}

// loadLadderCeiling reads the latest non-canceled run's snapshot joined with the
// config's current coverage. It refuses a run without a full snapshot.
func loadLadderCeiling(ctx context.Context, tx pgx.Tx, configID, oldAmount, newAmount string) (ladderCeilingSnapshot, error) {
	var snap ladderCeilingSnapshot
	var hasSnapshot bool
	err := tx.QueryRow(ctx, `SELECT r.started_at,
 r.target_usd_hr IS NOT NULL AND r.existing_usd_hr IS NOT NULL AND r.baseline_usd_hr IS NOT NULL,
 COALESCE(trunc(LEAST(r.target_usd_hr,r.baseline_usd_hr*c.target_coverage/100)-r.existing_usd_hr,6),0)::float8,
 $2::numeric::float8,$3::numeric::float8
 FROM ladder_runs r JOIN ladder_configs c ON c.id=r.config_id
 WHERE r.config_id=$1 AND r.status<>'cancelled' ORDER BY r.started_at DESC,r.created_at DESC,r.id DESC LIMIT 1`,
		configID, oldAmount, newAmount).Scan(&snap.anchor, &hasSnapshot, &snap.ceiling, &snap.oldAmount, &snap.newAmount)
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
