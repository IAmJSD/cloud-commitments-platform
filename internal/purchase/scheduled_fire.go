package purchase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// FireResult carries the aggregate outcome of a single FireScheduledDelayedPurchases
// sweep. Callers log these counts and return them to the Lambda scheduler so
// they appear in CloudWatch Logs.
type FireResult struct {
	// Found is the number of status=scheduled rows whose scheduled_execution_at
	// is in the past (SELECT pre-CAS).
	Found int `json:"found"`
	// Fired is the number of rows successfully transitioned from "scheduled" to
	// "approved" and then through executeAndFinalize. Fired <= Found.
	Fired int `json:"fired"`
	// RaceLost is the number of rows where the scheduled->approved CAS was
	// rejected by a concurrent operation (e.g. the user clicked Revoke between
	// the SELECT and the CAS). This is normal and not an error.
	RaceLost int `json:"race_lost"`
	// Errored is the number of rows where the fire attempt failed for a reason
	// other than a CAS race (DB error, provider error). Worth alerting on.
	Errored int `json:"errored"`
	// Stale is the number of claimed rows refused, unbought, because they were
	// due more than the due-row age cap ago (ErrDueRowTooOld, issue #712). They
	// end failed with no_submission and are not counted in Errored.
	Stale int `json:"stale"`
}

// fireOutcome is what fireOneDue did with one due row.
type fireOutcome int

const (
	fireErrored  fireOutcome = iota // a real error (DB, provider)
	fireFired                       // claimed and run through executeAndFinalize
	fireRaceLost                    // another actor owned the row first
	fireStale                       // claimed but refused by the due-row age cap
)

// FireScheduledDelayedPurchases runs a sweep that fires all purchase_executions
// with status="scheduled" and scheduled_execution_at <= NOW(). For each row:
//
//  1. Atomically transition scheduled -> approved via TransitionExecutionStatus
//     (CAS; skips the row if the revoke handler won the race and flipped it to
//     canceled first).
//  2. Stamp ApprovedBy = "scheduler" to preserve audit trail.
//  3. Run executeAndFinalize to call the cloud SDK and flip the row to
//     completed/failed.
//
// A claimed row whose scheduled_execution_at is more than the due-row age cap
// in the past is failed unbought instead (issue #712), so the first tick after
// the sweep is scheduled cannot fire a backlog of stale delayed purchases.
//
// Safety: idempotent across duplicate invocations (the CAS guard prevents
// double-firing). Per-row error isolation: a failure on row N never blocks
// rows N+1..K. Counts are aggregated in FireResult for the caller to log.
func (m *Manager) FireScheduledDelayedPurchases(ctx context.Context) (*FireResult, error) {
	due, err := m.config.GetScheduledExecutionsDue(ctx)
	if err != nil {
		return nil, fmt.Errorf("fire scheduled purchases: list due rows: %w", err)
	}

	result := &FireResult{Found: len(due)}
	if len(due) == 0 {
		return result, nil
	}

	logging.Infof("FireScheduledDelayedPurchases: found %d row(s) due for execution", len(due))

	for i := range due {
		switch m.fireOneDue(ctx, &due[i]) {
		case fireFired:
			result.Fired++
		case fireRaceLost:
			result.RaceLost++
		case fireStale:
			result.Stale++
		default:
			result.Errored++
		}
	}

	logging.Infof("FireScheduledDelayedPurchases: found=%d fired=%d race_lost=%d stale=%d errored=%d",
		result.Found, result.Fired, result.RaceLost, result.Stale, result.Errored)
	return result, nil
}

// fireOneDue attempts to fire a single due scheduled execution. It returns
// fireFired on success, fireRaceLost when the CAS was lost to a concurrent
// revoke or the claim was lost before the provider call (the guarded audit
// stamp found the row no longer running), fireStale when the claimed row was
// past the due-row age cap and was failed unbought, or fireErrored on a real
// error.
func (m *Manager) fireOneDue(ctx context.Context, exec *config.PurchaseExecution) fireOutcome {
	// CAS: scheduled -> running (the executor claim). If this fails with ErrExecutionNotInExpectedStatus
	// the revoke handler already transitioned the row to "canceled" — that is
	// not an error, just a CAS race loss.
	// Scheduler-initiated fire: no human session UUID, so transitioned_by = NULL.
	updated, err := m.claimRunning(ctx, exec.ExecutionID, []string{"scheduled"}, nil)
	if err != nil {
		if errors.Is(err, config.ErrExecutionNotInExpectedStatus) || errors.Is(err, config.ErrNotFound) {
			logging.Infof("fireOneDue[%s]: CAS lost (execution already transitioned by another actor)", exec.ExecutionID)
			return fireRaceLost
		}
		logging.Errorf("fireOneDue[%s]: TransitionExecutionStatus failed: %v", exec.ExecutionID, err)
		return fireErrored
	}

	// The age cap is checked on the claimed row (issue #712): this run now
	// owns it, so no other sweep can buy or fail it, and the refusal fails it
	// with no_submission and notifies operators exactly once.
	if refusal := dueRowAgeRefusal(updated, m.dueRowMaxAge(), time.Now()); refusal != nil {
		if failErr := m.failClaimedBeforeSubmission(ctx, updated, refusal); errors.Is(failErr, config.ErrAuditLoss) {
			// Nothing was bought, but the failed status was not saved: the row
			// stays running for the reaper, so this is a real error.
			logging.Errorf("fireOneDue[%s]: %v", exec.ExecutionID, failErr)
			return fireErrored
		}
		return fireStale
	}

	// Stamp ApprovedBy for the audit trail before executing.
	actor := "scheduler"
	updated.ApprovedBy = &actor
	// Guarded on "running". A store error stays best-effort, but a lost guard
	// means the claim is gone, so nothing is bought (issue #835).
	if stampErr := m.stampClaimedAudit(ctx, updated, "approved_by"); stampErr != nil {
		return fireRaceLost
	}

	if execErr := m.executeAndFinalize(ctx, updated); execErr != nil {
		logging.Errorf("fireOneDue[%s]: executeAndFinalize failed: %v", exec.ExecutionID, execErr)
		return fireErrored
	}

	return fireFired
}
