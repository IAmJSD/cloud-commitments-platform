package purchase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/email"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SendUpcomingPurchaseNotifications sends notifications for upcoming automated purchases.
func (m *Manager) SendUpcomingPurchaseNotifications(ctx context.Context) (*NotificationResult, error) {
	logging.Info("Checking for upcoming purchases to notify...")

	plans, err := m.config.ListPurchasePlans(ctx, config.PurchasePlanFilter{})
	if err != nil {
		return nil, fmt.Errorf("failed to list purchase plans: %w", err)
	}

	// Kill switch, read once per tick. A read error fails the tick rather than
	// defaulting to enabled (fail closed on the money path).
	autoPurchaseEnabled, err := m.autoPurchaseEnabled(ctx)
	if err != nil {
		return nil, err
	}

	notified := 0
	for _rvc := range plans {
		plan := plans[_rvc]
		if m.shouldNotifyPlan(plan, autoPurchaseEnabled) {
			if m.sendPlanNotification(ctx, &plan) {
				notified++
			}
		}
	}

	return &NotificationResult{
		Notified: notified,
	}, nil
}

// shouldNotifyPlan checks if a plan should trigger a notification.
// autoPurchaseEnabled is the global scheduling.auto_purchase kill switch.
func (m *Manager) shouldNotifyPlan(plan config.PurchasePlan, autoPurchaseEnabled bool) bool {
	if !autoPurchaseEnabled || !plan.Enabled || !plan.AutoPurchase {
		return false
	}

	if plan.NextExecutionDate == nil {
		return false
	}

	daysUntil := int(time.Until(*plan.NextExecutionDate).Hours() / config.HoursPerDay)
	if daysUntil < 0 || daysUntil > plan.NotificationDaysBefore {
		return false
	}

	// Check if we already sent notification recently
	if plan.LastNotificationSent != nil {
		hoursSinceNotification := time.Since(*plan.LastNotificationSent).Hours()
		if hoursSinceNotification < config.MinHoursBetweenNotifications {
			return false
		}
	}

	return true
}

// errExecutionNotNotifiable marks an existing execution for the plan's date
// that has already left pending/notified; every notification tick until the
// plan's NextExecutionDate advances hits it, so it is skipped quietly.
var errExecutionNotNotifiable = errors.New("execution is no longer awaiting approval")

// sendPlanNotification sends a notification for a plan and returns true if successful.
func (m *Manager) sendPlanNotification(ctx context.Context, plan *config.PurchasePlan) bool {
	daysUntil := int(time.Until(*plan.NextExecutionDate).Hours() / config.HoursPerDay)
	logging.Infof("Sending notification for plan %s (purchase in %d days)", plan.Name, daysUntil)

	// Resolve the notification recipient before touching any token. The
	// rendered body embeds action tokens and must be delivered via targeted
	// SES, not broadcast via SNS. Log a warning and skip rather than
	// broadcasting if no recipient is configured.
	notifyEmail := m.globalNotificationEmail(ctx)
	if notifyEmail == "" {
		logging.Warnf("Skipping scheduled-purchase notification for plan %s: no notification email configured in global settings", plan.Name)
		return false
	}

	execution, rawApprovalToken, rotationPending, err := m.getOrCreateExecution(ctx, plan)
	if errors.Is(err, errExecutionNotNotifiable) {
		logging.Debugf("Skipping notification for plan %s: %v", plan.Name, err)
		return false
	}
	if err != nil {
		logging.Errorf("Failed to create execution: %v", err)
		return false
	}

	data := m.buildNotificationData(*plan, execution, rawApprovalToken, daysUntil, notifyEmail)
	if err := m.email.SendScheduledPurchaseNotification(ctx, data); err != nil {
		logging.Errorf("Failed to send notification: %v", err)
		return false
	}

	// Persist a rotated token only after the email carrying it went out, so a
	// failed send leaves the previously emailed link live (issue #103). On
	// failure the timestamp below is not written and the next tick resends.
	if rotationPending {
		rotated, rotErr := m.config.RotatePendingApprovalToken(ctx, execution.ExecutionID, execution.ApprovalToken, *execution.ApprovalTokenExpiresAt)
		if rotErr != nil || !rotated {
			logging.Errorf("purchase[%s]: notification sent but its approval token was not persisted (rotated=%t): %v",
				execution.ExecutionID, rotated, rotErr)
			return false
		}
	}

	// Update notification sent time. If this fails the suppression timestamp is
	// not persisted, so the next run would resend — return false to avoid
	// double-counting and let the caller decide whether to retry.
	now := time.Now()
	if err := m.config.StampPlanNotificationSent(ctx, plan.ID, now); err != nil {
		logging.Errorf("Failed to update plan notification timestamp: %v", err)
		return false
	}

	return true
}

// globalNotificationEmail returns the configured global notification
// address, or "" when none is set or the config can't be read.
func (m *Manager) globalNotificationEmail(ctx context.Context) string {
	if globalCfg, cfgErr := m.config.GetGlobalConfig(ctx); cfgErr == nil && globalCfg != nil && globalCfg.NotificationEmail != nil {
		return *globalCfg.NotificationEmail
	}
	return ""
}

// getOrCreateExecution gets an existing execution or creates a new one, and
// returns the RAW approval token to embed in the notification about to be
// sent. Only the token's hash is stored (issue #103), so an existing row
// never carries an emailable token: a fresh one is minted into the returned
// copy (hash + ApprovalTokenTTL expiry) but NOT persisted, and
// rotationPending tells the caller to persist it via
// RotatePendingApprovalToken once the email has been sent. A new row is
// saved with its hash up front, since nothing was emailed for it yet.
func (m *Manager) getOrCreateExecution(ctx context.Context, plan *config.PurchasePlan) (execution *config.PurchaseExecution, rawToken string, rotationPending bool, err error) {
	// The whole decision runs under the plan's ramp lock (platform#631): two
	// ticks (SQS redelivery, overlapping cron, scheduler retries) otherwise
	// both read "nothing there" and both write a row for the same step. The
	// step, not the date, is the key, because rows created ahead of time carry
	// their own scheduled_date.
	var step stepClaim
	txErr := m.config.WithTx(ctx, func(tx pgx.Tx) error {
		var claimErr error
		step, claimErr = m.claimPlanStep(ctx, tx, plan)
		return claimErr
	})
	if txErr != nil {
		return nil, "", false, txErr
	}
	if step.failedNew {
		return nil, "", false, bareStepFailure(plan.ID, step.number)
	}
	if step.blocked != nil {
		return nil, "", false, step.blocked
	}
	existing := step.existing
	if len(existing.Recommendations) == 0 {
		return nil, "", false, m.failExistingBarePlanStep(ctx, existing)
	}
	tok, genErr := common.GenerateApprovalToken()
	if genErr != nil {
		return nil, "", false, fmt.Errorf("failed to generate approval token: %w", genErr)
	}
	expiry := time.Now().Add(config.ApprovalTokenTTL)
	existing.ApprovalToken = config.HashApprovalToken(tok)
	existing.ApprovalTokenExpiresAt = &expiry
	return existing, tok, true, nil
}

// stepClaim is the outcome of claimPlanStep: a freshly recorded failed row, a
// quiet skip error, or the pending root row to notify.
type stepClaim struct {
	number    int
	existing  *config.PurchaseExecution
	blocked   error
	failedNew bool
}

// claimPlanStep runs inside the tick's transaction: lock the plan, look at
// every row of the step, and either record the step as failed (nothing to
// buy yet) or hand back the one row that may be notified.
func (m *Manager) claimPlanStep(ctx context.Context, tx pgx.Tx, plan *config.PurchasePlan) (stepClaim, error) {
	locked, lockErr := m.config.LockPurchasePlanTx(ctx, tx, plan.ID)
	if lockErr != nil {
		return stepClaim{}, fmt.Errorf("failed to lock plan %s: %w", plan.ID, lockErr)
	}
	if locked == nil {
		return stepClaim{}, fmt.Errorf("plan not found: %s", plan.ID)
	}
	// step_number names the step this row will COMPLETE, not the count
	// already completed, matching api.createPurchaseExecutionsTx
	// (CurrentStep + 1). The ramp advance is keyed on it since #1669.
	claim := stepClaim{number: locked.RampSchedule.CurrentStep + 1}
	rows, listErr := m.config.ListExecutionsForPlanStepTx(ctx, tx, plan.ID, claim.number)
	if listErr != nil {
		return stepClaim{}, fmt.Errorf("failed to check for existing execution: %w", listErr)
	}
	if len(rows) > 0 {
		claim.existing, claim.blocked = adoptableRootRow(rows)
		return claim, nil
	}
	// Nothing attaches recommendations to a plan step yet (platform#609), so
	// the row is recorded as failed instead of emailing a $0 approval link.
	// The failed row also makes every later tick for this step return
	// errExecutionNotNotifiable.
	fresh := &config.PurchaseExecution{
		PlanID:        plan.ID,
		ExecutionID:   uuid.New().String(),
		Status:        "pending",
		StepNumber:    claim.number,
		ScheduledDate: *plan.NextExecutionDate,
	}
	stampFailedBarePlanStep(fresh)
	if saveErr := m.config.SavePurchaseExecutionTx(ctx, tx, fresh); saveErr != nil {
		return stepClaim{}, fmt.Errorf("failed to record plan step %d of plan %s as failed: %w", claim.number, plan.ID, saveErr)
	}
	claim.failedNew = true
	return claim, nil
}

// adoptableRootRow picks the row a notification tick may notify for a step
// that already has rows. Only a pending/notified ROOT row (no account, first
// attempt) qualifies: child and retry rows share the step number, and a step
// whose root is failed, partially_completed, expired, canceled or in flight
// must not get a second root, because approving it would buy the step again
// (E1/E2). The second return value is the quiet skip error when none qualifies.
func adoptableRootRow(rows []config.PurchaseExecution) (*config.PurchaseExecution, error) {
	for i := range rows {
		r := rows[i]
		if r.CloudAccountID == nil && r.RetryAttemptN == 0 && (r.Status == "pending" || r.Status == "notified") {
			return &r, nil
		}
	}
	first := rows[0]
	return nil, fmt.Errorf("%w: step %d of plan %s already has execution %s (%s)",
		errExecutionNotNotifiable, first.StepNumber, first.PlanID, first.ExecutionID, first.Status)
}

// failExistingBarePlanStep fails a pending/notified plan-step row that carries
// no recommendations. The status change is a compare-and-set, so a human
// approving the row between this tick's read and now wins: the row is no
// longer awaiting approval and the tick skips it, instead of an upsert from the
// stale read overwriting the approval. A won CAS returns the fresh row, which
// is the one the error text is written onto.
func (m *Manager) failExistingBarePlanStep(ctx context.Context, exec *config.PurchaseExecution) error {
	failed, err := m.config.TransitionExecutionStatus(ctx, exec.ExecutionID, []string{"pending", "notified"}, "failed", nil)
	if errors.Is(err, config.ErrNotFound) || errors.Is(err, config.ErrExecutionNotInExpectedStatus) {
		return fmt.Errorf("%w: %s changed state before it could be failed", errExecutionNotNotifiable, exec.ExecutionID)
	}
	if err != nil {
		return fmt.Errorf("failed to fail plan step %d of plan %s: %w", exec.StepNumber, exec.PlanID, err)
	}
	return m.recordFailedBarePlanStep(ctx, failed)
}

// recordFailedBarePlanStep stamps the failure reason onto exec (already failed,
// or a new row), saves it, logs it once, and returns errExecutionNotNotifiable
// so the notification tick sends nothing for it.
func (m *Manager) recordFailedBarePlanStep(ctx context.Context, exec *config.PurchaseExecution) error {
	stampFailedBarePlanStep(exec)
	if err := m.config.SavePurchaseExecution(ctx, exec); err != nil {
		return fmt.Errorf("failed to record plan step %d of plan %s as failed: %w", exec.StepNumber, exec.PlanID, err)
	}
	return bareStepFailure(exec.PlanID, exec.StepNumber)
}

// stampFailedBarePlanStep marks exec failed with the no-recommendations reason
// and logs it once.
func stampFailedBarePlanStep(exec *config.PurchaseExecution) {
	exec.Status = "failed"
	exec.NoSubmission = true // a pending/notified bare step never reaches the executor
	exec.Error = ErrPlanStepNoRecommendations.Error()
	logging.Errorf("purchase[%s]: plan %s step %d has no recommendations and was marked failed: %v",
		exec.ExecutionID, exec.PlanID, exec.StepNumber, ErrPlanStepNoRecommendations)
}

// bareStepFailure is the quiet skip error a tick returns after failing a bare
// step, so it sends nothing for it.
func bareStepFailure(planID string, step int) error {
	return fmt.Errorf("%w: step %d of plan %s has no recommendations", errExecutionNotNotifiable, step, planID)
}

// buildNotificationData creates notification data from plan and execution.
// notifyEmail is the global notification address from GlobalConfig; it is set
// as RecipientEmail so the token-bearing body routes through targeted SES.
// rawApprovalToken is the RAW token from getOrCreateExecution -- never read
// from exec.ApprovalToken, which holds only the hash (issue #103).
func (m *Manager) buildNotificationData(plan config.PurchasePlan, exec *config.PurchaseExecution, rawApprovalToken string, daysUntil int, notifyEmail string) email.NotificationData {
	data := email.NotificationData{
		DashboardURL:      m.dashboardURL,
		ApprovalToken:     rawApprovalToken,
		ExecutionID:       exec.ExecutionID,
		PlanID:            plan.ID,
		TotalSavings:      exec.EstimatedSavings,
		TotalUpfrontCost:  exec.TotalUpfrontCost,
		PurchaseDate:      exec.ScheduledDate.Format("January 2, 2006"),
		DaysUntilPurchase: daysUntil,
		PlanName:          plan.Name,
		RecipientEmail:    notifyEmail,
	}

	for _rvc := range exec.Recommendations {
		rec := exec.Recommendations[_rvc]
		data.Recommendations = append(data.Recommendations, email.RecommendationSummary{
			Service:        rec.Service,
			ResourceType:   rec.ResourceType,
			Engine:         rec.Engine,
			Region:         rec.Region,
			Count:          rec.Count,
			MonthlySavings: rec.Savings,
		})
	}

	return data
}
