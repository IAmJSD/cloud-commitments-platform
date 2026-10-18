package purchase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// executorOwnedStatuses is the only status an executor may write its terminal
// result or audit stamps over: it owns the row while it holds the "running"
// claim and not after the reaper, a cancel or another writer moved it on.
var executorOwnedStatuses = []string{"running"}

// ErrClaimedRowRefused marks a run refused by the caller's ClaimedRowCheck
// after the claim was won and before any provider call (issue #415). The row
// ends "failed" with no_submission stamped; the check's own error is wrapped
// alongside it so callers can still map it (for example to a 403).
var ErrClaimedRowRefused = errors.New("purchase refused after claim: the claimed execution no longer passes the authorization checks")

// ClaimedRowCheck re-runs a caller's authorization checks (permission
// Constraints such as MaxPurchaseAmount) against the row the claim CAS
// returned, which is exactly the row executeAndFinalize buys. A caller's
// earlier check ran on a snapshot read before the CAS, so a repricing or a
// recommendation attach landing in between would otherwise be bought
// unchecked (issue #415). A nil check skips the re-check; only callers with
// no pre-CAS Constraints check (the SQS approve worker) pass nil.
type ClaimedRowCheck func(ctx context.Context, claimed *config.PurchaseExecution) error

// claimRunning is the single claim primitive: an atomic CAS from one of from
// to "running". Winning it is the right to call the provider; every executor
// (approve, scheduler, SQS, cron, re-drive) claims before any provider call.
// A lost CAS returns an error satisfying
// errors.Is(err, config.ErrExecutionNotInExpectedStatus) or config.ErrNotFound.
// actor is the transitioned_by user UUID, nil for system-initiated claims.
func (m *Manager) claimRunning(ctx context.Context, executionID string, from []string, actor *string) (*config.PurchaseExecution, error) {
	return m.config.TransitionExecutionStatus(ctx, executionID, from, "running", actor)
}

// claimAndCheck claims the row for transitionApproveAndExecute and then runs
// check against the claimed row. A lost claim returns (nil, err). A refused
// check fails the claimed row before submission and returns (row, err), with
// err satisfying errors.Is(err, ErrClaimedRowRefused); nothing is bought.
func (m *Manager) claimAndCheck(ctx context.Context, executionID string, from []string, actor *string, check ClaimedRowCheck, t0 time.Time) (*config.PurchaseExecution, error) {
	updated, err := m.claimRunning(ctx, executionID, from, actor)
	if err != nil {
		logging.Errorf("purchase[%s]: transitionApproveAndExecute status transition failed after %s: %v",
			executionID, time.Since(t0), err)
		return nil, fmt.Errorf("approve: %w", err)
	}
	if check == nil {
		return updated, nil
	}
	if checkErr := check(ctx, updated); checkErr != nil {
		return updated, m.failClaimedBeforeSubmission(ctx, updated, fmt.Errorf("%w: %w", ErrClaimedRowRefused, checkErr))
	}
	return updated, nil
}
