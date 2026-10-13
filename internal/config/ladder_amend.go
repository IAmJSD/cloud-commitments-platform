package config

import (
	"context"
	"errors"
	"math/big"
	"regexp"
	"time"
)

var (
	ErrLadderAmendNotFound = errors.New("ladder tranche not found")
	ErrLadderAmendConflict = errors.New("ladder tranche changed or is no longer editable")
	ErrLadderAmendInvalid  = errors.New("ladder amendment exceeds the original run total, the current per-run cap or the config commitment ceiling")
	ladderAmountPattern    = regexp.MustCompile(`^(0|[1-9]\d{0,13})(\.\d{1,6})?$`)
)

// ladderAmendValidationError keeps the Validate message for the caller while
// matching ErrLadderAmendInvalid, so store callers can map every invalid
// amendment to a client error with errors.Is.
type ladderAmendValidationError struct{ err error }

func (e ladderAmendValidationError) Error() string   { return e.err.Error() }
func (e ladderAmendValidationError) Unwrap() []error { return []error{ErrLadderAmendInvalid, e.err} }

type LadderAmendment struct {
	ExpectedRevision *int64    `json:"expected_revision"`
	ScheduledDate    time.Time `json:"scheduled_date"`
	AmountUSDHr      string    `json:"amount_usd_hr"`
}

func (a LadderAmendment) Validate() error {
	amount, ok := new(big.Rat).SetString(a.AmountUSDHr)
	if !ladderAmountPattern.MatchString(a.AmountUSDHr) || !ok || amount.Sign() <= 0 {
		return errors.New("amount_usd_hr must be a positive decimal with at most six fractional digits")
	}
	if a.ExpectedRevision == nil || *a.ExpectedRevision < 0 || a.ScheduledDate.IsZero() {
		return errors.New("expected_revision must be nonnegative and scheduled_date is required")
	}
	return nil
}

type LadderAmendmentResult struct {
	ID            string    `json:"id"`
	Revision      int64     `json:"revision"`
	ScheduledDate time.Time `json:"scheduled_date"`
	AmountUSDHr   string    `json:"amount_usd_hr"`
	RunTotalUSDHr string    `json:"run_total_usd_hr"`
}

type LadderAmendmentStore interface {
	LadderTrancheScope(context.Context, string) (string, string, error)
	AmendLadderTranche(context.Context, string, string, string, string, LadderAmendment) (*LadderAmendmentResult, error)
}
