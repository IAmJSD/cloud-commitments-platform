package purchase

import (
	"context"
	"errors"
	"fmt"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// recoveredAfterReapNote is the executor's own audit note for a row it had to
// recover; it does not depend on the reaper's annotation, which may land after
// or never.
const recoveredAfterReapNote = "recovered after reap; provider purchase succeeded"

// saveOrRecoverTerminal persists the finalized row guarded on "running". When
// the guard fails because the reaper already flipped the row to failed while the
// provider call went on to succeed, the outcome is real money: it recovers the
// row to completed/partially_completed (unless a retry successor is already
// linked) so the ramp can advance and the commitment is not re-bought. Every
// other guard failure is returned as-is for the caller to wrap as audit loss.
func (m *Manager) saveOrRecoverTerminal(ctx context.Context, exec *config.PurchaseExecution, execErr error) error {
	err := m.saveTerminalExecution(ctx, exec, execErr)
	if err == nil || !errors.Is(err, config.ErrExecutionNotInExpectedStatus) || !purchasedOutcome(exec) {
		return err
	}
	exec.Error = appendErrNote(exec.Error, recoveredAfterReapNote)
	recErr := m.config.RecoverReapedExecution(ctx, exec)
	switch {
	case recErr == nil:
		logging.Warnf("purchase[%s]: terminal save found the row reaped; recovered to %s", exec.ExecutionID, exec.Status)
		return nil
	case errors.Is(recErr, config.ErrExecutionRetryLinked):
		return fmt.Errorf("POSSIBLE DOUBLE BUY, operator review needed: purchase succeeded but the reaped row already has a retry successor: %w", recErr)
	default:
		return fmt.Errorf("%w; recovery failed: %w", err, recErr)
	}
}

func purchasedOutcome(exec *config.PurchaseExecution) bool {
	return (exec.Status == "completed" || exec.Status == "partially_completed") && anyRecPurchased(exec.Recommendations)
}
