package config

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Outcomes of an operator resolve of a stale processing RI exchange (#692)
// that the store distinguishes. The handler maps them to 404, 400 and 409.
var (
	ErrRIExchangeResolveNotFound = errors.New("ri exchange not found")
	ErrRIExchangeResolveInvalid  = errors.New("invalid ri exchange resolve request")
	ErrRIExchangeResolveConflict = errors.New("ri exchange cannot be resolved")
)

// RIExchangeResolveStaleAfter is how long a processing RI exchange must have
// been unchanged before an operator may resolve it. Every writer that can
// still settle a processing row (the approval request, the scheduled AUTO
// run) finishes within its own budget: 4m30s on the API path and the task
// timeout (15m default, TASK_TIMEOUT on Fargate) on the scheduled path. The
// margin keeps a late settlement from racing an operator's decision, and
// matches the ladder's fired-tranche threshold.
const RIExchangeResolveStaleAfter = 6 * time.Hour

// riExchangeResolveNoteMax bounds the note stored in the error column.
const riExchangeResolveNoteMax = 1000

// RIExchangeCloudCheck is the operator's statement of where they verified the
// exchange's outcome at AWS before resolving it.
type RIExchangeCloudCheck struct {
	AccountID string
	Region    string
	CheckedAt time.Time
}

// RIExchangeResolve asks to settle a stale processing RI exchange on an
// operator's word. Outcome is StatusCompleted (the exchange happened: the row
// keeps its reserved amount and counts on today's UTC day) or StatusFailed
// (the exchange did not happen: the hold is released). Note is the complete
// text stored in the error column, built by the caller so that a repeat of the
// same request compares equal as a whole.
type RIExchangeResolve struct {
	ID         string
	Outcome    string
	ExchangeID string
	Note       string
	Check      RIExchangeCloudCheck
	Actor      *string
	StaleAfter time.Duration
}

// RIExchangeResolveResult is the row after the call. Replayed is true when an
// identical earlier request had already resolved it and nothing was written.
type RIExchangeResolveResult struct {
	ID         string
	Status     string
	PaymentDue string
	Note       string
	Replayed   bool
}

// RIExchangeResolveStore is the store surface of the operator resolve endpoint.
type RIExchangeResolveStore interface {
	ResolveStaleRIExchange(ctx context.Context, req RIExchangeResolve) (*RIExchangeResolveResult, error)
}

var _ RIExchangeResolveStore = (*PostgresStore)(nil)

// riExchangeResolveRow is the locked row the resolve rules are checked against.
type riExchangeResolveRow struct {
	status, accountID, region, exchangeID, note, paymentDue string
	updatedAt, dbNow                                        time.Time
}

// ResolveStaleRIExchange settles a processing RI exchange that no running
// writer can still settle. It locks the row and refuses unless it is still
// processing, has been unchanged for StaleAfter (measured on the database
// clock), and the operator's check names the row's own account and region and
// postdates its last change. Releasing (failed) is also refused when the row
// carries a provider exchange id, since AWS then reported the exchange done.
// A row a late writer completed or failed is never overwritten.
func (s *PostgresStore) ResolveStaleRIExchange(ctx context.Context, req RIExchangeResolve) (*RIExchangeResolveResult, error) {
	if err := validateRIExchangeResolve(req); err != nil {
		return nil, err
	}
	res := &RIExchangeResolveResult{ID: req.ID, Status: req.Outcome, Note: req.Note}
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		row, err := lockRIExchangeForResolve(ctx, tx, req.ID)
		if err != nil {
			return err
		}
		if row.status != "processing" {
			return replayRIExchangeResolve(req, row, res)
		}
		if err := checkRIExchangeResolve(req, row); err != nil {
			return err
		}
		return applyRIExchangeResolve(ctx, tx, req, res)
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func validateRIExchangeResolve(req RIExchangeResolve) error {
	if req.StaleAfter <= 0 {
		return errors.New("ri exchange resolve needs a positive StaleAfter")
	}
	switch {
	case req.Outcome != StatusCompleted && req.Outcome != StatusFailed:
		return fmt.Errorf("%w: outcome must be %q or %q", ErrRIExchangeResolveInvalid, StatusCompleted, StatusFailed)
	case req.Outcome == StatusFailed && req.ExchangeID != "":
		return fmt.Errorf("%w: an exchange id means the exchange happened; resolve it as completed", ErrRIExchangeResolveInvalid)
	case req.Note == "" || len(req.Note) > riExchangeResolveNoteMax:
		return fmt.Errorf("%w: note must be 1 to %d bytes", ErrRIExchangeResolveInvalid, riExchangeResolveNoteMax)
	case !req.Check.complete():
		return fmt.Errorf("%w: the cloud check must name an account, a region and a time", ErrRIExchangeResolveInvalid)
	}
	return nil
}

func (c RIExchangeCloudCheck) complete() bool {
	return c.AccountID != "" && c.Region != "" && !c.CheckedAt.IsZero()
}

func lockRIExchangeForResolve(ctx context.Context, tx pgx.Tx, id string) (riExchangeResolveRow, error) {
	var row riExchangeResolveRow
	err := tx.QueryRow(ctx, `
		SELECT status, account_id, region, COALESCE(exchange_id, ''), COALESCE(error, ''),
		       payment_due::text, updated_at, NOW()
		FROM ri_exchange_history
		WHERE id = $1
		FOR UPDATE`, id,
	).Scan(&row.status, &row.accountID, &row.region, &row.exchangeID, &row.note, &row.paymentDue, &row.updatedAt, &row.dbNow)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, ErrRIExchangeResolveNotFound
	}
	if err != nil {
		return row, fmt.Errorf("failed to lock ri exchange %s: %w", id, err)
	}
	return row, nil
}

// replayRIExchangeResolve answers a request that found the row no longer
// processing: success only when it holds exactly what this request would have
// written, else a conflict naming the current status.
func replayRIExchangeResolve(req RIExchangeResolve, row riExchangeResolveRow, res *RIExchangeResolveResult) error {
	if row.status == req.Outcome && row.note == req.Note {
		res.PaymentDue = row.paymentDue
		res.Replayed = true
		return nil
	}
	return fmt.Errorf("%w: exchange is %s, not processing", ErrRIExchangeResolveConflict, row.status)
}

func checkRIExchangeResolve(req RIExchangeResolve, row riExchangeResolveRow) error {
	switch {
	case row.dbNow.Sub(row.updatedAt) <= req.StaleAfter:
		return fmt.Errorf("%w: the exchange last changed at %s and may still be in flight; wait until it is %s old",
			ErrRIExchangeResolveConflict, row.updatedAt.UTC().Format(time.RFC3339), req.StaleAfter)
	case row.exchangeID == "":
		// No provider exchange id: nothing below applies.
	case req.Outcome == StatusFailed:
		return fmt.Errorf("%w: the exchange has provider exchange id %q, so it happened; it cannot be released",
			ErrRIExchangeResolveConflict, row.exchangeID)
	case req.ExchangeID != "" && req.ExchangeID != row.exchangeID:
		return fmt.Errorf("%w: exchange_id does not match the recorded provider exchange id", ErrRIExchangeResolveInvalid)
	}
	return checkRIExchangeCloudCheck(req.Check, row)
}

// checkRIExchangeCloudCheck requires the operator's check to name the row's own
// account and region and to postdate its last change.
func checkRIExchangeCloudCheck(c RIExchangeCloudCheck, row riExchangeResolveRow) error {
	switch {
	case c.AccountID != row.accountID:
		return fmt.Errorf("%w: checked_account_id is not the exchange's account", ErrRIExchangeResolveInvalid)
	case c.Region != row.region:
		return fmt.Errorf("%w: checked_region is not the exchange's region", ErrRIExchangeResolveInvalid)
	case c.CheckedAt.After(row.dbNow):
		return fmt.Errorf("%w: checked_at is in the future", ErrRIExchangeResolveInvalid)
	case !checkPostdatesDBTime(c.CheckedAt, row.updatedAt):
		return fmt.Errorf("%w: checked_at must be after the exchange's last change at %s",
			ErrRIExchangeResolveInvalid, row.updatedAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// checkPostdatesDBTime reports whether an operator's check time is strictly
// after last, a timestamp read back from Postgres. Postgres keeps microseconds,
// so the check is compared at that precision too: a check taken within the
// same microsecond as the change cannot be ordered after it, and comparing the
// raw nanoseconds would let it through.
func checkPostdatesDBTime(checked, last time.Time) bool {
	return checked.Truncate(time.Microsecond).After(last)
}

// applyRIExchangeResolve writes the outcome. completed keeps payment_due (the
// reserved ceiling, an upper bound of what AWS accepted) and stamps
// completed_at, so the row counts on today's UTC day like any other settled
// exchange; failed releases the hold.
func applyRIExchangeResolve(ctx context.Context, tx pgx.Tx, req RIExchangeResolve, res *RIExchangeResolveResult) error {
	err := tx.QueryRow(ctx, `
		UPDATE ri_exchange_history
		SET status = $2::text,
		    error = $3,
		    exchange_id = CASE WHEN $4::text = '' THEN exchange_id ELSE $4::text END,
		    completed_at = CASE WHEN $2::text = 'completed' THEN NOW() ELSE completed_at END,
		    transitioned_by = $5::uuid,
		    transitioned_at = NOW(),
		    updated_at = NOW()
		WHERE id = $1 AND status = 'processing'
		RETURNING payment_due::text`,
		req.ID, req.Outcome, req.Note, req.ExchangeID, req.Actor,
	).Scan(&res.PaymentDue)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: exchange changed while resolving", ErrRIExchangeResolveConflict)
	}
	if err != nil {
		return fmt.Errorf("failed to resolve ri exchange %s: %w", req.ID, err)
	}
	return nil
}
