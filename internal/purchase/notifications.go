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

	global, err := m.config.GetGlobalConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read global config for the overdue-plan grace window: %w", err)
	}

	notified := 0
	for _rvc := range plans {
		plan := plans[_rvc]
		if err := m.failDueManualStep(ctx, &plan); err != nil {
			logging.Errorf("Failed to inspect due manual plan step for plan %s: %v", plan.ID, err)
			continue
		}
		if m.shouldNotifyPlan(plan, autoPurchaseEnabled) || shouldRetryOverduePlan(plan, autoPurchaseEnabled, global, time.Now()) {
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
	if !plan.Enabled || (plan.AutoPurchase && !autoPurchaseEnabled) {
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

// planGraceDays is the longest grace window among the plan's providers.
func planGraceDays(plan *config.PurchasePlan, global *config.GlobalConfig) int {
	grace := 0
	for _, p := range planProviders(plan) {
		grace = max(grace, global.GracePeriodFor(p))
	}
	return grace
}

// shouldRetryOverduePlan admits a plan whose next date has passed while its
// step may still be waiting for a matching recommendation: from the date until
// one day past the grace window, so the first tick at or after the deadline can
// write the failed row. A step that already has a row is skipped by the claim.
func shouldRetryOverduePlan(plan config.PurchasePlan, autoPurchaseEnabled bool, global *config.GlobalConfig, now time.Time) bool {
	if !plan.Enabled || (plan.AutoPurchase && !autoPurchaseEnabled) || plan.NextExecutionDate == nil || !plan.NextExecutionDate.Before(now) {
		return false
	}
	if plan.LastNotificationSent != nil && now.Sub(*plan.LastNotificationSent).Hours() < config.MinHoursBetweenNotifications {
		return false
	}
	return now.Before(plan.NextExecutionDate.AddDate(0, 0, planGraceDays(&plan, global)+1))
}

func (m *Manager) failDueManualStep(ctx context.Context, plan *config.PurchasePlan) error {
	if !plan.Enabled || plan.AutoPurchase {
		return nil
	}
	var root *config.PurchaseExecution
	if err := m.config.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		root, err = m.dueManualRootTx(ctx, tx, plan.ID)
		return err
	}); err != nil {
		return fmt.Errorf("failed to inspect due manual plan step: %w", err)
	}
	if root == nil {
		return nil
	}
	err := m.failExistingPlanStep(ctx, root, "scheduled plan step reached its scheduled date without recommendations; no purchase was submitted")
	if errors.Is(err, errExecutionNotNotifiable) {
		return nil
	}
	return err
}

func (m *Manager) dueManualRootTx(ctx context.Context, tx pgx.Tx, planID string) (*config.PurchaseExecution, error) {
	locked, err := m.config.LockPurchasePlanTx(ctx, tx, planID)
	if err != nil {
		return nil, err
	}
	if !enabledManualPlan(locked) {
		return nil, nil
	}
	if locked.RampSchedule.DatesManaged {
		return m.dueManagedPlanRootTx(ctx, tx, locked)
	}
	rows, err := m.config.ListExecutionsForPlanStepTx(ctx, tx, planID, locked.RampSchedule.CurrentStep+1)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	candidate, blocked := adoptableRootRow(rows)
	if blocked == nil && len(candidate.Recommendations) == 0 && !candidate.ScheduledDate.After(time.Now()) {
		return candidate, nil
	}
	return nil, nil
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

	// The row's own date, not the plan's: an attach for an overdue plan is
	// scheduled NotificationDaysBefore days out.
	daysUntil = int(time.Until(execution.ScheduledDate).Hours() / config.HoursPerDay)
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

// getOrCreateExecution returns the execution to notify for the plan's next
// step and the RAW approval token to embed in the notification about to be
// sent. Only the token's hash is stored (issue #103): the token is minted into
// the returned copy but NOT persisted, and rotationPending tells the caller to
// persist it via RotatePendingApprovalToken once the email went out, which
// also promotes the row to 'notified'. A step without recommendations gets
// them attached first (attachStep); see plan_step_attach.go.
func (m *Manager) getOrCreateExecution(ctx context.Context, plan *config.PurchasePlan) (execution *config.PurchaseExecution, rawToken string, rotationPending bool, err error) {
	var step stepClaim
	txErr := m.config.WithTx(ctx, func(tx pgx.Tx) error {
		var claimErr error
		step, claimErr = m.claimPlanStep(ctx, tx, plan)
		return claimErr
	})
	if txErr != nil {
		return nil, "", false, txErr
	}
	if step.failedNew != nil {
		// Notified only once the claim's transaction committed the row; the
		// plan lock lets one tick record it (issue #1195).
		m.notifyExecutionFailure(ctx, step.failedNew)
		return nil, "", false, bareStepFailure(plan.ID, step.number, step.failureReason)
	}
	if step.targetMet {
		return nil, "", false, fmt.Errorf("%w: step %d of plan %s already has its target and was completed as a no-op", errExecutionNotNotifiable, step.number, plan.ID)
	}
	if step.blocked != nil {
		return nil, "", false, step.blocked
	}
	if step.emptyRoot != nil {
		// The claim left an empty pre-created row to fail by compare-and-set,
		// outside the transaction.
		return nil, "", false, m.failExistingPlanStep(ctx, step.emptyRoot, step.failureReason)
	}
	existing := step.existing
	tok, genErr := common.GenerateApprovalToken()
	if genErr != nil {
		return nil, "", false, fmt.Errorf("failed to generate approval token: %w", genErr)
	}
	expiry := time.Now().Add(config.ApprovalTokenTTL)
	existing.ApprovalToken = config.HashApprovalToken(tok)
	existing.ApprovalTokenExpiresAt = &expiry
	return existing, tok, true, nil
}

// stepClaim is the outcome of claimPlanStep: a freshly recorded failed row, an
// empty pre-created row to fail, a quiet skip error, or the row to notify.
type stepClaim struct {
	number        int
	existing      *config.PurchaseExecution
	emptyRoot     *config.PurchaseExecution
	blocked       error
	failedNew     *config.PurchaseExecution
	targetMet     bool
	failureReason string
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

// failExistingPlanStep fails a pending/notified plan-step row that will never
// be bought, with reason. The CAS pins the recommendations to what the caller
// saw (empty for a bare row, non-empty for a refused attached one). The status change is a compare-and-set,
// so a human approving the row between this tick's read and now wins: the row
// is no longer awaiting approval and the tick skips it, instead of an upsert
// from the stale read overwriting the approval. A won CAS returns the fresh
// row, which is the one the error text is written onto.
//
// The CAS, the Error/NoSubmission stamp and the suppression release commit in
// one transaction (issue #1052). A failed root is never adopted again, so a
// CAS committed on its own could strand the row failed with no reason and its
// suppressions still held; an error before commit now leaves the row pending
// for the next tick.
func (m *Manager) failExistingPlanStep(ctx context.Context, exec *config.PurchaseExecution, reason string) error {
	var failed *config.PurchaseExecution
	err := m.config.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		failed, txErr = m.failPlanStepTx(ctx, tx, exec, reason)
		return txErr
	})
	if errors.Is(err, errExecutionNotNotifiable) {
		return err
	}
	if err != nil {
		return fmt.Errorf("failed to fail plan step %d of plan %s: %w", exec.StepNumber, exec.PlanID, err)
	}
	logFailedPlanStep(failed)
	// Notified only once the transaction committed the failed row: a lost CAS
	// or an error before commit leaves it pending and quiet (issue #1195).
	m.notifyExecutionFailure(ctx, failed)
	return bareStepFailure(failed.PlanID, failed.StepNumber, reason)
}

// failPlanStepTx is failExistingPlanStep's transaction: the pinned CAS, then
// the stamp and the suppression release on the row it returned. Only a lost
// CAS is the quiet changed-state skip; any later error is a failed write.
func (m *Manager) failPlanStepTx(ctx context.Context, tx pgx.Tx, exec *config.PurchaseExecution, reason string) (*config.PurchaseExecution, error) {
	failed, err := m.config.TransitionExecutionStatusPinnedTx(ctx, tx, exec.ExecutionID, []string{"pending", "notified"}, "failed", nil, len(exec.Recommendations) == 0)
	if errors.Is(err, config.ErrNotFound) || errors.Is(err, config.ErrExecutionNotInExpectedStatus) {
		return nil, fmt.Errorf("%w: %s changed state before it could be failed", errExecutionNotNotifiable, exec.ExecutionID)
	}
	if err != nil {
		return nil, err
	}
	markFailedPlanStep(failed, reason)
	// The save is guarded on "failed", the status this CAS set (issue #835).
	// The CAS's row lock already keeps other writers out until commit, so this
	// is defense in depth: the full-row write never puts back a status another
	// writer set, and a lost guard rolls back the CAS and the release with it.
	if err := m.config.SaveExecutionIfStatusTx(ctx, tx, failed, []string{config.StatusFailed}); err != nil {
		return nil, fmt.Errorf("failed to record plan step %d of plan %s as failed: %w", failed.StepNumber, failed.PlanID, err)
	}
	// The row is failed with NoSubmission, so its suppressions are released as
	// for any proven not-submitted failure (saveTerminalExecution); a bare row has none.
	if err := m.config.DeleteSuppressionsByExecutionTx(ctx, tx, failed.ExecutionID); err != nil {
		return nil, err
	}
	return failed, nil
}

// stampFailedPlanStep marks exec failed with reason and logs it once.
func stampFailedPlanStep(exec *config.PurchaseExecution, reason string) {
	markFailedPlanStep(exec, reason)
	logFailedPlanStep(exec)
}

// markFailedPlanStep sets the failed status, NoSubmission and reason on exec.
func markFailedPlanStep(exec *config.PurchaseExecution, reason string) {
	exec.Status = "failed"
	exec.NoSubmission = true // a pending/notified step never reaches the executor
	exec.Error = reason
}

// logFailedPlanStep logs a plan step that was marked failed.
func logFailedPlanStep(exec *config.PurchaseExecution) {
	logging.Errorf("purchase[%s]: plan %s step %d was marked failed: %s", exec.ExecutionID, exec.PlanID, exec.StepNumber, exec.Error)
}

// stampFailedBarePlanStep marks exec failed with the no-recommendations reason.
func stampFailedBarePlanStep(exec *config.PurchaseExecution) {
	stampFailedPlanStep(exec, ErrPlanStepNoRecommendations.Error())
}

// bareStepFailure is the quiet skip error a tick returns after failing a
// step, so it sends nothing for it.
func bareStepFailure(planID string, step int, reason string) error {
	return fmt.Errorf("%w: step %d of plan %s failed: %s", errExecutionNotNotifiable, step, planID, reason)
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

	if plan.MaxStepCommitment != nil {
		data.StepCap = *plan.MaxStepCommitment
	}
	for _rvc := range exec.Recommendations {
		rec := exec.Recommendations[_rvc]
		summary := email.RecommendationSummary{
			Service:          rec.Service,
			ResourceType:     rec.ResourceType,
			Engine:           rec.Engine,
			Region:           rec.Region,
			Payment:          rec.Payment,
			Term:             rec.Term,
			Count:            rec.Count,
			MonthlySavings:   rec.Savings,
			UpfrontCost:      rec.UpfrontCost,
			Commitment:       rec.TotalCommitment(),
			AvailabilityZone: rec.AvailabilityZone,
		}
		var recurring float64
		if rec.MonthlyCost != nil {
			summary.MonthlyCost = *rec.MonthlyCost
			recurring = *rec.MonthlyCost * float64(rec.Term*12)
		}
		// Platform and tenancy are display only; a Details blob that does not
		// decode is logged and shown without them (the cap and purchase paths
		// decode it themselves and fail loud).
		if ci, err := rec.ComputeIdentity(); err != nil {
			logging.Warnf("purchase[%s]: notification row without platform/tenancy: %v", exec.ExecutionID, err)
		} else {
			summary.Platform, summary.Tenancy = ci.Platform, ci.Tenancy
		}
		data.TotalCommitment += summary.Commitment
		data.TotalRecurring += recurring
		data.TotalUnits += rec.Count
		data.Recommendations = append(data.Recommendations, summary)
	}

	return data
}
