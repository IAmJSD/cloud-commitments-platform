package purchase

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/ctxutil"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/email"
)

// PurchaseFailedLogMarker is the stable token logged once for every execution
// row that ends "failed" or "partially_completed" (issue #714). The per-cloud
// monitoring modules (terraform/modules/monitoring/{aws,gcp,azure}) turn it
// into a log-based metric and alert on any occurrence, so the token is a
// contract with Terraform: rename it only together with those filters
// (TestMonitoringLogMarkersMatchGo guards the pairing).
const PurchaseFailedLogMarker = "purchase_execution_failed"

// failureNotifyTimeout bounds the recipient lookup and the send. The context is
// detached from the caller's, which may already be near its deadline after a
// long purchase run.
const failureNotifyTimeout = 30 * time.Second

// notifyExecutionFailure tells operators that an execution row did not fully
// succeed: it logs PurchaseFailedLogMarker (the alarm source) and sends the
// purchase-failed email listing the selected recommendations that did not
// purchase. A completed (or any non-terminal) row is a no-op.
//
// It runs after the terminal status is decided, once per row, from every
// writer that ends a row failed or partially_completed (issue #1195):
//   - executeAndFinalize, for the root row (single-account and pre-fan-out
//     failures; a fan-out root gets the marker only, see notifyRootFailure);
//   - executeForAccount, for each per-account fan-out row;
//   - failClaimedBeforeSubmission, for a claimed row refused before submission;
//   - safeFail and the reaper, for stranded rows the sweeps fail;
//   - failExistingPlanStep and getOrCreateExecution, for a plan step that is
//     failed without ever reaching the executor.
//
// Each of them notifies only after winning the row: the "running" claim, or the
// compare-and-set (or plan lock) that moved it to failed, so concurrent workers
// cannot both notify for the same row.
//
// Best effort: a send failure is logged and never changes the purchase outcome.
func (m *Manager) notifyExecutionFailure(ctx context.Context, exec *config.PurchaseExecution) {
	if !logExecutionFailure(exec) || m.email == nil {
		return
	}
	ctx, cancel := ctxutil.DetachedWithTimeout(ctx, failureNotifyTimeout)
	defer cancel()
	data := m.buildPurchaseFailedData(ctx, exec)
	if err := m.email.SendPurchaseFailedNotification(ctx, data); err != nil {
		logging.Errorf("Failed to send purchase-failed notification for execution %s: %v", exec.ExecutionID, err)
	}
}

// buildPurchaseFailedData lists the selected recommendations that did not
// purchase and addresses the email to the global notification address (the
// same recipient the scheduled-purchase notice uses). An empty recipient lets
// the sender fall back to its broadcast channel.
func (m *Manager) buildPurchaseFailedData(ctx context.Context, exec *config.PurchaseExecution) email.NotificationData {
	data := email.NotificationData{
		DashboardURL:   strings.TrimRight(m.dashboardURL, "/"),
		ExecutionID:    exec.ExecutionID,
		PlanID:         exec.PlanID,
		RecipientEmail: m.globalNotificationEmail(ctx),
	}
	for i := range exec.Recommendations {
		rec := exec.Recommendations[i]
		if !rec.Selected || rec.Purchased {
			continue
		}
		data.Recommendations = append(data.Recommendations, email.RecommendationSummary{
			Service:      rec.Service,
			ResourceType: rec.ResourceType,
			Engine:       rec.Engine,
			Region:       rec.Region,
			Count:        rec.Count,
		})
	}
	return data
}

// logExecutionFailure logs PurchaseFailedLogMarker, the alarm source, when exec
// ended failed or partially_completed, and reports whether it did.
func logExecutionFailure(exec *config.PurchaseExecution) bool {
	if exec.Status != "failed" && exec.Status != "partially_completed" {
		return false
	}
	logging.Errorf("%s execution_id=%s plan_id=%s status=%s",
		PurchaseFailedLogMarker, exec.ExecutionID, exec.PlanID, exec.Status)
	return true
}

// notifyRootFailure is executeAndFinalize's notification for the root row. A
// fan-out root always logs the marker but sends no email: the per-account rows
// email for themselves, and the root could not list the failed recs (it never
// carries per-rec Purchased flags). The marker cannot be left to the children,
// because a child can fail without reaching its own notification: the fan-out
// skips an account once the context is done, and a panicking child is
// recovered with its row still non-terminal (issue #1195).
func (m *Manager) notifyRootFailure(ctx context.Context, exec *config.PurchaseExecution, execErr error) {
	if isFanOutOutcome(execErr) {
		logExecutionFailure(exec)
		return
	}
	m.notifyExecutionFailure(ctx, exec)
}

// isFanOutOutcome reports whether execErr is the aggregate of a multi-account
// fan-out, whose root row gets the failure marker but no email (see
// notifyRootFailure).
func isFanOutOutcome(execErr error) bool {
	var partial *multiAccountPartialError
	return errors.Is(execErr, errAllAccountsFailed) || errors.As(execErr, &partial)
}
