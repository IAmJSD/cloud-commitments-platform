package purchase

import (
	"context"
	"errors"
	"fmt"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// strandedRecoveryError is the operator-facing error a stranded approved row is
// failed with when the sweep does not re-drive it. The interrupted run may
// already have bought, so it always tells the operator to check the provider.
func strandedRecoveryError(exec *config.PurchaseExecution) string {
	next := "Verify on the cloud provider that no commitment was created, then Retry."
	if RedriveRefusalReason(exec) != "" {
		next = "Retry is refused for this purchase because a repeat could buy a second commitment. Verify on the cloud provider whether the commitment was created; if it was not, start a new purchase."
	}
	return "execution was approved but its purchase run was interrupted before completing and never finalized; failed by the recovery sweep so it is not silently stuck (issue #632). " + next
}

// redrivable reports whether RecoverStrandedApprovals may re-drive exec in
// place: every recommendation is safe to re-drive and the row has a durable
// ExecutionID for the idempotency lineage. The sweep checks it on the listed
// snapshot to pick a path, and claimAndRedrive checks it again on the claimed
// row before buying (issue #1037).
func redrivable(exec *config.PurchaseExecution) bool {
	return allRecsSafeToRedrive(exec) && exec.ExecutionID != ""
}

// failClaimedRedrive fails a stranded row claimAndRedrive has claimed
// ("running") but whose claimed state no longer qualifies for a re-drive, for
// example because its recommendations were edited to an EC2 one between the
// sweep's SELECT and the claim. The outcome is the one safeFail gives a row
// listed in that state: failed with the recovery explanation. It is NOT a
// beforeSubmission refusal: the interrupted original run may already have
// bought, so no_submission is not stamped and plan suppressions are kept.
// Returns (true, nil) when the row was failed, matching safeFail's count, and
// an ErrAuditLoss error when the terminal save fails (the row would then sit
// in "running" for the reaper).
func (m *Manager) failClaimedRedrive(ctx context.Context, exec *config.PurchaseExecution) (bool, error) {
	cause := errors.New(strandedRecoveryError(exec))
	logging.Errorf("Recovering stranded approved execution %s: the claimed row no longer qualifies for an in-place re-drive; failing it for visibility", exec.ExecutionID)
	m.finalizeExecution(exec, cause)
	if err := m.saveTerminalExecution(ctx, exec, cause); err != nil {
		return false, fmt.Errorf("%w: failed to save stranded execution %s as failed after claiming it (row stranded in running): %w",
			config.ErrAuditLoss, exec.ExecutionID, err)
	}
	return true, nil
}
