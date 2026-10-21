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
// It runs after the terminal status is decided, once per row: the root row in
// executeAndFinalize (single-account and pre-fan-out failures) and each
// per-account row in executeForAccount (multi-account fan-out, whose root never
// carries per-rec Purchased flags). Every caller holds the row's "running"
// claim, so concurrent workers cannot both notify for the same row.
//
// Best effort: a send failure is logged and never changes the purchase outcome.
func (m *Manager) notifyExecutionFailure(ctx context.Context, exec *config.PurchaseExecution) {
	if exec.Status != "failed" && exec.Status != "partially_completed" {
		return
	}
	logging.Errorf("%s execution_id=%s plan_id=%s status=%s",
		PurchaseFailedLogMarker, exec.ExecutionID, exec.PlanID, exec.Status)
	if m.email == nil {
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

// isFanOutOutcome reports whether execErr is the aggregate of a multi-account
// fan-out. Each per-account row already notified for itself, so the root row
// must not notify again (and could not list the failed recs: the root never
// carries per-rec Purchased flags).
func isFanOutOutcome(execErr error) bool {
	var partial *multiAccountPartialError
	return errors.Is(execErr, errAllAccountsFailed) || errors.As(execErr, &partial)
}
