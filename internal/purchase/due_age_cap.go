package purchase

import (
	"errors"
	"fmt"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// DefaultMaxDueRowAge is how long after its due time the fire and process
// sweeps may still buy a row when ManagerConfig.MaxDueRowAge is unset. It
// matches config.LadderFireGrace, the same limit the ladder already applies to
// its own scheduled tranches.
const DefaultMaxDueRowAge = 24 * time.Hour

// ErrDueRowTooOld marks a row the fire or process sweep refused because it was
// due more than the due-row age cap ago (issue #712). The first tick after a
// sweep is enabled (the fire sweep by default since #712, the process sweep
// on opt-in) can find a long backlog of overdue rows whose recommendations
// and prices are stale; buying them all at once is the outcome this cap
// prevents. The row ends failed with no_submission set, so it can be retried
// deliberately from History.
var ErrDueRowTooOld = errors.New("scheduled purchase skipped: it was due too long ago")

// dueRowMaxAge is the sweeps' age cap, DefaultMaxDueRowAge when unset.
func (m *Manager) dueRowMaxAge() time.Duration {
	if m.maxDueRowAge > 0 {
		return m.maxDueRowAge
	}
	return DefaultMaxDueRowAge
}

// dueAt is when the sweeps consider exec due: scheduled_execution_at for a
// delayed (status "scheduled") purchase, which only the delayed-approve path
// sets, and scheduled_date for every other row.
func dueAt(exec *config.PurchaseExecution) time.Time {
	if exec.ScheduledExecutionAt != nil {
		return *exec.ScheduledExecutionAt
	}
	return exec.ScheduledDate
}

// dueRowAgeRefusal refuses exec when it was due more than maxAge before now.
// Callers run it on the row their "running" claim returned, right before the
// provider call: the claim already shut out every other executor, so a row
// cannot age past the cap between this check and the purchase. The message is
// what History shows as the row's error.
func dueRowAgeRefusal(exec *config.PurchaseExecution, maxAge time.Duration, now time.Time) error {
	due := dueAt(exec)
	late := now.Sub(due)
	if late <= maxAge {
		return nil
	}
	return fmt.Errorf("%w: it was due at %s, %s ago, which is more than the %s limit (SCHEDULED_PURCHASE_MAX_OVERDUE); nothing was submitted, retry it if the purchase is still wanted",
		ErrDueRowTooOld, due.UTC().Format(time.RFC3339), late.Round(time.Minute), maxAge)
}
