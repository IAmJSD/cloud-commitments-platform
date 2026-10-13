package config

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/jackc/pgx/v5"
)

func (s *PostgresStore) LadderTrancheScope(ctx context.Context, id string) (accountID, provider string, err error) {
	err = s.db.QueryRow(ctx, `SELECT c.cloud_account_id,c.provider FROM ladder_tranches t
 JOIN ladder_configs c ON c.id=t.config_id
 JOIN ladder_runs r ON r.id=t.run_id AND r.config_id=c.id
 JOIN cloud_accounts a ON a.id=c.cloud_account_id AND a.provider=c.provider
 WHERE t.id=$1`, id).Scan(&accountID, &provider)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrLadderAmendNotFound
	}
	return accountID, provider, err
}

func (s *PostgresStore) AmendLadderTranche(ctx context.Context, id, accountID, provider, actor string, amendment LadderAmendment) (*LadderAmendmentResult, error) {
	if err := amendment.Validate(); err != nil {
		return nil, ladderAmendValidationError{err}
	}
	if actor == "" {
		return nil, errors.New("ladder amendment actor is required")
	}
	configID, err := s.ladderTrancheConfigID(ctx, id, accountID, provider)
	if err != nil {
		return nil, fmt.Errorf("amend ladder tranche: %w", err)
	}
	result := &LadderAmendmentResult{ID: id}
	// The ladder lock is taken first, row locks after (see withLadderConfigLock),
	// so the ceiling sum below is read against the planner's committed state.
	err = s.withLadderConfigLock(ctx, configID, func(tx pgx.Tx) error {
		return amendLadderTrancheTx(ctx, tx, configID, id, accountID, provider, actor, amendment, result)
	})
	if err != nil {
		return nil, fmt.Errorf("amend ladder tranche: %w", err)
	}
	return result, nil
}

type lockedLadderRun struct {
	configID, runID, originalTotal string
	cap                            *string
	status                         ladder.RunStatus
}

type lockedLadderTranche struct {
	status      ladder.TrancheStatus
	executionID *string
	revision    int64
	amount      string
	date        time.Time
	future      bool
}

func (t lockedLadderTranche) editable(runStatus ladder.RunStatus, expectedRevision int64) bool {
	return runStatus == ladder.RunStatusPlanned && t.status == ladder.TrancheStatusScheduled &&
		t.executionID == nil && t.revision == expectedRevision && t.future
}

// ladderTrancheConfigID resolves the tranche's config without any row lock, so
// the caller can take the config's ladder lock before locking rows.
func (s *PostgresStore) ladderTrancheConfigID(ctx context.Context, id, accountID, provider string) (string, error) {
	var configID string
	err := s.db.QueryRow(ctx, `SELECT t.config_id FROM ladder_tranches t
 JOIN ladder_configs c ON c.id=t.config_id WHERE t.id=$1 AND c.cloud_account_id=$2 AND c.provider=$3`, id, accountID, provider).Scan(&configID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrLadderAmendNotFound
	}
	return configID, err
}

func amendLadderTrancheTx(ctx context.Context, tx pgx.Tx, configID, id, accountID, provider, actor string, amendment LadderAmendment, result *LadderAmendmentResult) error {
	run, err := lockLadderRun(ctx, tx, id, accountID, provider)
	if err != nil {
		return err
	}
	if run.configID != configID {
		return ErrLadderAmendConflict
	}
	tranche, err := lockLadderTranche(ctx, tx, id, run, amendment.ScheduledDate)
	if err != nil {
		return err
	}
	if !tranche.editable(run.status, *amendment.ExpectedRevision) {
		return ErrLadderAmendConflict
	}
	err = checkLadderRunTotal(ctx, tx, run, tranche.amount, amendment.AmountUSDHr, result)
	if err != nil {
		return err
	}
	err = checkLadderConfigCeiling(ctx, tx, run.configID, tranche.amount, amendment.AmountUSDHr)
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `UPDATE ladder_tranches SET amount_usd_hr=$2::numeric,scheduled_date=$3,revision=revision+1
 WHERE id=$1 AND revision=$4 AND scheduled_date>clock_timestamp() AND $3::timestamptz>clock_timestamp()
 RETURNING revision,amount_usd_hr::text,scheduled_date`, id, amendment.AmountUSDHr, amendment.ScheduledDate, amendment.ExpectedRevision).
		Scan(&result.Revision, &result.AmountUSDHr, &result.ScheduledDate)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLadderAmendConflict
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO ladder_tranche_amendments
 (tranche_id,actor,previous_revision,new_revision,previous_amount_usd_hr,new_amount_usd_hr,previous_scheduled_date,new_scheduled_date)
 VALUES ($1,$2,$3,$4,$5::numeric,$6::numeric,$7,$8)`, id, actor, tranche.revision, result.Revision, tranche.amount, result.AmountUSDHr, tranche.date, result.ScheduledDate)
	return err
}

func lockLadderRun(ctx context.Context, tx pgx.Tx, id, accountID, provider string) (lockedLadderRun, error) {
	var run lockedLadderRun
	err := tx.QueryRow(ctx, `SELECT c.id,c.max_hourly_commit_per_run::text
 FROM ladder_configs c JOIN cloud_accounts a ON a.id=c.cloud_account_id AND a.provider=c.provider
 WHERE c.cloud_account_id=$2 AND c.provider=$3
 AND c.id=(SELECT config_id FROM ladder_tranches WHERE id=$1) FOR NO KEY UPDATE OF c`, id, accountID, provider).
		Scan(&run.configID, &run.cap)
	if errors.Is(err, pgx.ErrNoRows) {
		return run, ErrLadderAmendNotFound
	}
	if err != nil {
		return run, err
	}
	err = tx.QueryRow(ctx, `SELECT id,status,total_hourly_commit::text FROM ladder_runs
 WHERE config_id=$2 AND id=(SELECT run_id FROM ladder_tranches WHERE id=$1) FOR NO KEY UPDATE`, id, run.configID).
		Scan(&run.runID, &run.status, &run.originalTotal)
	if errors.Is(err, pgx.ErrNoRows) {
		return run, ErrLadderAmendNotFound
	}
	return run, err
}

func lockLadderTranche(ctx context.Context, tx pgx.Tx, id string, run lockedLadderRun, proposed time.Time) (lockedLadderTranche, error) {
	var t lockedLadderTranche
	err := tx.QueryRow(ctx, `SELECT status,execution_id,revision,amount_usd_hr::text,scheduled_date,
 scheduled_date>clock_timestamp() AND $4::timestamptz>clock_timestamp()
 FROM ladder_tranches WHERE id=$1 AND config_id=$2 AND run_id=$3 FOR UPDATE`, id, run.configID, run.runID, proposed).
		Scan(&t.status, &t.executionID, &t.revision, &t.amount, &t.date, &t.future)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrLadderAmendNotFound
	}
	return t, err
}

func checkLadderRunTotal(ctx context.Context, tx pgx.Tx, run lockedLadderRun, oldAmount, newAmount string, result *LadderAmendmentResult) error {
	var allowed bool
	err := tx.QueryRow(ctx, `SELECT total::text,$3::numeric<=$2::numeric OR (total<=$4::numeric AND ($5::numeric IS NULL OR total<=$5::numeric))
 FROM (SELECT COALESCE(SUM(amount_usd_hr),0)-$2::numeric+$3::numeric AS total
 FROM ladder_tranches WHERE run_id=$1) totals`, run.runID, oldAmount, newAmount, run.originalTotal, run.cap).
		Scan(&result.RunTotalUSDHr, &allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrLadderAmendInvalid
	}
	return nil
}
