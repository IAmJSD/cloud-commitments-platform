package purchase

import (
	"context"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// executorOwnedStatuses is the only status an executor may write its terminal
// result or audit stamps over: it owns the row while it holds the "running"
// claim and not after the reaper, a cancel or another writer moved it on.
var executorOwnedStatuses = []string{"running"}

// claimRunning is the single claim primitive: an atomic CAS from one of from
// to "running". Winning it is the right to call the provider; every executor
// (approve, scheduler, SQS, cron, re-drive) claims before any provider call.
// A lost CAS returns an error satisfying
// errors.Is(err, config.ErrExecutionNotInExpectedStatus) or config.ErrNotFound.
// actor is the transitioned_by user UUID, nil for system-initiated claims.
func (m *Manager) claimRunning(ctx context.Context, executionID string, from []string, actor *string) (*config.PurchaseExecution, error) {
	return m.config.TransitionExecutionStatus(ctx, executionID, from, "running", actor)
}
