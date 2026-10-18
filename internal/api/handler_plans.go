// Package api provides the HTTP API handlers for the CUDly dashboard.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/planstep"
	"github.com/aws/aws-lambda-go/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Plans handlers.
func (h *Handler) listPlans(ctx context.Context, req *events.LambdaFunctionURLRequest, params map[string]string) (*PlansResponse, error) {
	session, err := h.requirePermission(ctx, req, "view", "plans")
	if err != nil {
		return nil, err
	}

	// parseAccountIDs validates and splits the comma-separated account_ids
	// query param. Returns nil (no filter) when absent or empty.
	accountIDs, err := parseAccountIDs(params["account_ids"])
	if err != nil {
		return nil, NewClientError(400, err.Error())
	}

	filter, anyAccount, err := h.scopedPlanFilter(ctx, session, accountIDs)
	if err != nil {
		return nil, err
	}
	if !anyAccount {
		return &PlansResponse{Plans: []PlanWithHealth{}}, nil
	}
	plans, err := h.config.ListPurchasePlans(ctx, filter)
	if err != nil {
		return nil, err
	}

	// Ensure all enabled plans have NextExecutionDate calculated
	now := time.Now()
	for i := range plans {
		if plans[i].Enabled && plans[i].NextExecutionDate == nil {
			plans[i].NextExecutionDate = calculateNextExecutionDate(&plans[i], now)
		}
	}

	return &PlansResponse{Plans: h.attachPlanHealth(ctx, plans, now)}, nil
}

// scopedPlanFilter narrows account_ids to the session's allowed_accounts. Only
// unrestricted callers get unassigned plans, matching requirePlanAccess.
func (h *Handler) scopedPlanFilter(ctx context.Context, session *Session, accountIDs []string) (filter config.PurchasePlanFilter, anyAccount bool, err error) {
	scope, err := h.getAccountScope(ctx, session)
	if err != nil {
		return filter, false, fmt.Errorf("failed to get allowed accounts: %w", err)
	}
	if scope.AllowsAll() {
		return config.PurchasePlanFilter{AccountIDs: accountIDs, IncludeUnassigned: true}, true, nil
	}
	allowed, _, err := h.intersectAccountFilterScope(ctx, session, accountIDs, nil)
	if err != nil {
		return filter, false, err
	}
	return config.PurchasePlanFilter{AccountIDs: allowed}, len(allowed) > 0, nil
}

// attachPlanHealth computes the per-plan health-score badge (issue #340
// follow-up) for every plan in the list, from exact per-plan execution
// counts over the trailing planHealthLookbackDays
// (config.StoreInterface.CountExecutionsByPlanAndStatus, aggregated in SQL)
// rather than from a capped page of execution rows, which would understate
// plans whose executions fall outside the newest page.
//
// If the counts fetch fails, every plan's HealthScore is left nil, which
// serializes as `"health_score": null` and renders as an explicit "unknown"
// badge. A score is a number operators make purchasing decisions from, so
// an uncomputable one must stay absent: substituting a default (100 reads
// as "healthy", 0 as "broken") would be a fabricated number indistinguishable
// from a real one. The request itself still succeeds -- the Plans page is
// not primarily about execution history, so a 500 here would be a worse
// trade than a visibly-unknown badge.
func (h *Handler) attachPlanHealth(ctx context.Context, plans []config.PurchasePlan, now time.Time) []PlanWithHealth {
	result := make([]PlanWithHealth, len(plans))
	for i := range plans {
		result[i] = PlanWithHealth{PurchasePlan: plans[i]}
	}
	if len(plans) == 0 {
		return result
	}

	since := now.AddDate(0, 0, -planHealthLookbackDays)
	countsByPlan, err := h.config.CountExecutionsByPlanAndStatus(ctx, planHealthExecutionStatuses, since)
	if err != nil {
		logging.Warnf("listPlans: CountExecutionsByPlanAndStatus failed, plan health reported as unknown: %v", err)
		return result
	}

	// A blocked ramp is worth up to 25 points, so a score computed without it
	// is not a partially-informed score, it is a wrong one -- and it errs
	// healthy, on plans that are stopped. Withhold every score rather than
	// publish that, exactly as the counts fetch above does.
	stuckByPlan, err := h.config.GetStuckRampSteps(ctx)
	if err != nil {
		logging.Warnf("listPlans: GetStuckRampSteps failed, plan health reported as unknown: %v", err)
		return result
	}

	for i := range plans {
		score, factors := computePlanHealth(plans[i], now, countsByPlan[plans[i].ID], stuckByPlan[plans[i].ID])
		result[i].HealthScore = &score
		result[i].HealthFactors = factors
	}
	return result
}

// calculateNextExecutionDate calculates the next execution date for a plan.
func calculateNextExecutionDate(plan *config.PurchasePlan, now time.Time) *time.Time {
	var nextDate time.Time
	if plan.RampSchedule.Type == "immediate" {
		// For immediate, schedule for tomorrow
		nextDate = now.AddDate(0, 0, 1)
	} else if plan.RampSchedule.StepIntervalDays > 0 {
		// For other schedules, use the step interval from now
		nextDate = now.AddDate(0, 0, plan.RampSchedule.StepIntervalDays)
	} else {
		// Default to tomorrow
		nextDate = now.AddDate(0, 0, 1)
	}
	return &nextDate
}

// mapCreatePlanStorageError keeps storage details out of API responses.
func mapCreatePlanStorageError(err error, notFoundMsg, genericMsg, logFmt string, logArgs ...any) error {
	if errors.Is(err, config.ErrNotFound) {
		return NewClientError(http.StatusNotFound, notFoundMsg)
	}
	if errors.Is(err, config.ErrPurchasePlanConflict) {
		return NewClientError(http.StatusConflict, "plan changed; refresh the plan and try again")
	}
	if errors.Is(err, config.ErrEditorReconciliationRequired) {
		return editorReconciliationError()
	}
	logging.Errorf(logFmt, logArgs...)
	return NewClientError(http.StatusInternalServerError, genericMsg)
}

func (h *Handler) createPlan(ctx context.Context, httpReq *events.LambdaFunctionURLRequest) (any, error) {
	// Require create:plans permission
	session, err := h.requirePermission(ctx, httpReq, "create", "plans")
	if err != nil {
		return nil, err
	}

	var req PlanRequest
	if err := json.Unmarshal([]byte(httpReq.Body), &req); err != nil {
		return nil, NewClientError(400, "invalid request body")
	}

	// target_accounts is required: a plan must be tied to at least one
	// cloud_account row. The historical "leave blank to mean all accounts of
	// this provider" behavior created "universal plans" (rows in
	// purchase_plans with no matching plan_accounts row) that were hard to
	// scope, hard to filter, and hard to govern. Reject early with a clear
	// 400 so the frontend can surface the error before any DB write.
	if err := validateTargetAccounts(req.TargetAccounts); err != nil {
		return nil, err
	}

	// Refuse before the plan row exists: a scoped caller must not create a
	// plan that buys for an account outside their allowed_accounts.
	if err := h.requireAccountsAccess(ctx, session, req.TargetAccounts); err != nil {
		return nil, err
	}

	plan := req.toPurchasePlan()

	// Validate the plan
	if err := h.validateAndGatePlan(ctx, httpReq, session, nil, plan); err != nil {
		return nil, err
	}

	if err := h.config.CreatePurchasePlan(ctx, plan); err != nil {
		return nil, mapCreatePlanStorageError(err,
			"plan not found", "failed to create plan",
			"createPlan: CreatePurchasePlan failed (provider=%s service=%s accounts=%d): %v",
			req.Provider, req.Service, len(req.TargetAccounts), err)
	}

	// Provider-match validation + plan_accounts insert. SetPlanAccounts is
	// transactional internally, but the plan-row insert above is not part of
	// that tx. If either step here fails we roll the plan row back so the
	// invariant "every purchase_plans row has at least one plan_accounts
	// row" holds end-to-end.
	rollbackPlan := func() {
		if delErr := h.config.DeletePurchasePlan(ctx, plan.ID); delErr != nil {
			logging.Warnf("createPlan rollback: failed to delete partial plan %s: %v (manual cleanup may be required)", plan.ID, delErr)
		}
	}

	if err := h.validatePlanAccountProviders(ctx, plan.ID, req.TargetAccounts); err != nil {
		rollbackPlan()
		return nil, err
	}

	if err := h.config.SetPlanAccounts(ctx, plan.ID, req.TargetAccounts); err != nil {
		rollbackPlan()
		return nil, mapCreatePlanStorageError(err,
			"account not found", "failed to assign accounts to plan",
			"createPlan: SetPlanAccounts failed (plan=%s accounts=%d): %v",
			plan.ID, len(req.TargetAccounts), err)
	}

	return plan, nil
}

// validateTargetAccounts rejects a missing/empty target_accounts payload and
// rejects entries that are not valid UUIDs. Mirrors the validation the
// dedicated PUT /plans/:id/accounts endpoint already performs (see
// setPlanAccounts in handler_accounts.go) so a request that gets past
// createPlan would also get past that endpoint.
func validateTargetAccounts(ids []string) error {
	if len(ids) == 0 {
		return NewClientError(400, "target_accounts is required: a plan must be tied to at least one account")
	}
	for _, aid := range ids {
		if err := validateUUID(aid); err != nil {
			return NewClientError(400, fmt.Sprintf("invalid target_account %q: must be a valid UUID", aid))
		}
	}
	return nil
}

func (h *Handler) getPlan(ctx context.Context, req *events.LambdaFunctionURLRequest, planID string) (any, error) {
	// Validate UUID format to prevent injection attacks
	if err := validateUUID(planID); err != nil {
		return nil, err
	}

	session, err := h.requirePermission(ctx, req, "view", "plans")
	if err != nil {
		return nil, err
	}

	err = h.requirePlanAccess(ctx, session, planID)
	if err != nil {
		return nil, err
	}

	// Map a missing plan to 404 (mirrors updatePlan / patchPlan /
	// getPlanForPurchaseCreation). GetPurchasePlan wraps config.ErrNotFound,
	// which the router's IsNotFoundError check does NOT recognize (it matches
	// only the api-package sentinel), so a raw return here surfaced a deleted
	// plan as a 500. A scheduled-purchase row can outlive its plan (issue
	// #1403: ON DELETE SET NULL detaches the execution, or the user's account
	// scope changes), and the Edit button then loads GET /plans/{id}; the
	// frontend needs a 404 to distinguish "plan is gone" from a server error
	// and reconcile the stale row instead of dead-ending on a generic message.
	plan, err := h.config.GetPurchasePlan(ctx, planID)
	if err != nil {
		return nil, mapCreatePlanStorageError(err,
			"plan not found", "failed to load plan",
			"getPlan: GetPurchasePlan failed")
	}

	// Ensure plan has NextExecutionDate calculated
	if plan.Enabled && plan.NextExecutionDate == nil {
		plan.NextExecutionDate = calculateNextExecutionDate(plan, time.Now())
	}

	return plan, nil
}

func (h *Handler) updatePlan(ctx context.Context, httpReq *events.LambdaFunctionURLRequest, planID string) (any, error) {
	// Validate UUID format to prevent injection attacks
	if err := validateUUID(planID); err != nil {
		return nil, err
	}

	session, err := h.requirePermission(ctx, httpReq, "update", "plans")
	if err != nil {
		return nil, err
	}

	err = h.requirePlanMutationAccess(ctx, session, planID)
	if err != nil {
		return nil, err
	}

	var req PlanRequest
	err = json.Unmarshal([]byte(httpReq.Body), &req)
	if err != nil {
		return nil, NewClientError(400, "invalid request body")
	}
	var capField struct {
		MaxStepCommitment json.RawMessage `json:"max_step_commitment"`
	}
	if err = json.Unmarshal([]byte(httpReq.Body), &capField); err != nil {
		return nil, NewClientError(400, "invalid request body")
	}

	// Fetch existing plan to preserve data not sent in request. Do NOT wrap
	// err with the raw plan UUID: a non-ClientError propagates to the router's
	// logging.Errorf("API error: %v"), leaking the plan UUID and the raw DB
	// error string (issue #965, same shape as the createPlan-side fix).
	existingPlan, err := h.config.GetPurchasePlan(ctx, planID)
	if err != nil {
		return nil, mapCreatePlanStorageError(err,
			"plan not found", "failed to load plan",
			"updatePlan: GetPurchasePlan failed")
	}

	plan, err := mergeUpdatedPlan(existingPlan, &req, planID, capField.MaxStepCommitment != nil)
	if err != nil {
		return nil, err
	}

	if err := h.requirePlanArmGrant(ctx, httpReq, session, existingPlan, plan); err != nil {
		return nil, err
	}

	if err := h.config.UpdatePurchasePlan(ctx, plan); err != nil {
		return nil, mapCreatePlanStorageError(err, "plan not found", "failed to update plan", "updatePlan: UpdatePurchasePlan failed")
	}

	return plan, nil
}

// mergeUpdatedPlan builds the plan an update request describes, carrying over
// the state the request cannot express (timestamps, ramp progress, services)
// from the stored plan, and validates it.
func mergeUpdatedPlan(existingPlan *config.PurchasePlan, req *PlanRequest, planID string, capPresent bool) (*config.PurchasePlan, error) {
	// Create new plan from request
	plan := req.toPurchasePlan()
	plan.ID = planID

	// Preserve timestamps from existing plan
	plan.CreatedAt = existingPlan.CreatedAt
	plan.UpdatedAt = existingPlan.UpdatedAt

	// toPurchasePlan always rebuilds the ramp schedule from scratch
	// (CurrentStep=0, StartDate=now) and recomputes NextExecutionDate,
	// since it has no way to see the existing plan. Left alone, every PUT
	// -- including a bare rename -- silently restarts an in-progress ramp,
	// so the scheduler re-buys steps that already executed (issue #219).
	if err := preserveRampProgress(existingPlan, plan); err != nil {
		return nil, err
	}

	if req.MaxStepCommitment == nil && !capPresent {
		plan.MaxStepCommitment = existingPlan.MaxStepCommitment
	}
	if capPresent && req.MaxStepCommitment == nil && plan.AutoPurchase {
		return nil, NewClientError(400, "max_step_commitment cannot be cleared while auto_purchase is enabled")
	}

	// Fold the request's single service into the stored map rather than
	// replacing it, so other services survive the edit (issue #934).
	services, err := mergeRequestedServices(existingPlan.Services, plan.Services)
	if err != nil {
		return nil, err
	}
	plan.Services = services

	// Validate the plan
	if err := plan.Validate(); err != nil {
		return nil, NewClientError(400, fmt.Sprintf("validation error: %s", err))
	}
	return plan, nil
}

// preserveRampProgress carries ramp-in-progress state from the existing
// plan onto a freshly rebuilt one (issue #219). LastExecutionDate and
// LastNotificationSent record real purchase history, so they are always
// carried over; a rebuild has no way to know either and would otherwise
// wipe them. CurrentStep, StartDate, and NextExecutionDate are carried
// over when the requested ramp schedule has the same shape as the existing
// one (same type/percent/interval/step-count): an unrelated field edit
// (rename, target coverage, ...) resubmits the same schedule and must not
// restart it. A different shape is refused once a step has completed,
// because the rebuilt schedule would restart at step zero and forget the
// percentage already bought (issue #386); before the first step it is a
// plain schedule change.
func preserveRampProgress(existingPlan, plan *config.PurchasePlan) error {
	plan.LastExecutionDate = existingPlan.LastExecutionDate
	plan.LastNotificationSent = existingPlan.LastNotificationSent

	if rampScheduleShapeUnchanged(existingPlan.RampSchedule, plan.RampSchedule) {
		plan.RampSchedule.CurrentStep = existingPlan.RampSchedule.CurrentStep
		plan.RampSchedule.StartDate = existingPlan.RampSchedule.StartDate
		// Editor-owned markers the request cannot express; dropping them hands
		// the plan back to the interval formula.
		plan.RampSchedule.DatesManaged = existingPlan.RampSchedule.DatesManaged
		plan.RampSchedule.AllocationMode = existingPlan.RampSchedule.AllocationMode
		plan.NextExecutionDate = existingPlan.NextExecutionDate
		return nil
	}
	if existingPlan.RampSchedule.CurrentStep > 0 {
		return NewClientError(http.StatusConflict, fmt.Sprintf(
			"cannot change the ramp schedule of a plan with %d completed step(s); "+
				"resend the current ramp_schedule to keep it, or create a new plan",
			existingPlan.RampSchedule.CurrentStep))
	}
	return nil
}

// rampScheduleShapeUnchanged compares the schedule's shape, deliberately
// excluding the progress fields CurrentStep and StartDate, to tell apart
// "the update resubmitted the same schedule" from "the update picked a
// genuinely different one".
func rampScheduleShapeUnchanged(a, b config.RampSchedule) bool {
	return a.Type == b.Type &&
		a.PercentPerStep == b.PercentPerStep &&
		a.StepIntervalDays == b.StepIntervalDays &&
		a.TotalSteps == b.TotalSteps
}

func (h *Handler) deletePlan(ctx context.Context, req *events.LambdaFunctionURLRequest, planID string) (any, error) {
	// Validate UUID format to prevent injection attacks
	if err := validateUUID(planID); err != nil {
		return nil, err
	}

	session, err := h.requirePermission(ctx, req, "delete", "plans")
	if err != nil {
		return nil, err
	}

	if err := h.requirePlanMutationAccess(ctx, session, planID); err != nil {
		return nil, err
	}

	if err := h.config.DeletePurchasePlan(ctx, planID); err != nil {
		if errors.Is(err, config.ErrManagedPlanLiveExecutions) {
			return nil, planEditorRefusal(http.StatusConflict, codeManagedPlanLiveExecutions,
				"this plan still has active or unsettled purchases; cancel or revoke them first")
		}
		return nil, err
	}

	return map[string]string{"status": "deleted"}, nil
}

func (h *Handler) createPlannedPurchases(ctx context.Context, httpReq *events.LambdaFunctionURLRequest, planID string) (*CreatePlannedPurchasesResponse, error) {
	if err := validateUUID(planID); err != nil {
		return nil, err
	}

	// Require update:purchases — creating scheduled purchase rows is a
	// purchase-write operation. Standard Users (DefaultUserPermissions)
	// hold this verb; Plan Authors (create/update/delete:plans only)
	// do not, closing issue #1406 / #1418. The prior gate ("create","plans")
	// incorrectly admitted Plan Authors because they hold create:plans.
	session, err := h.requirePermission(ctx, httpReq, "update", "purchases")
	if err != nil {
		return nil, err
	}

	err = h.requirePlanMutationAccess(ctx, session, planID)
	if err != nil {
		return nil, err
	}

	req, startDate, err := h.parseCreatePurchasesRequest(httpReq.Body)
	if err != nil {
		return nil, err
	}

	if _, err = h.getPlanForPurchaseCreation(ctx, planID); err != nil {
		return nil, err
	}

	created := 0
	err = h.config.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		created, txErr = h.createPlannedPurchasesTx(ctx, tx, planID, req.Count, startDate, resolveCreatorUserID(session), session)
		return txErr
	})
	if err != nil {
		clientErr, isClient := IsClientError(err)
		if isClient {
			return nil, clientErr
		}
		if errors.Is(err, errNotFound) {
			return nil, err
		}
		return nil, mapCreatePlanStorageError(err, "plan not found", "failed to schedule purchases", "createPlannedPurchases: transaction failed: %v", err)
	}
	return &CreatePlannedPurchasesResponse{Created: created}, nil
}

// createPlannedPurchasesTx is the transactional body of createPlannedPurchases:
// take the per-plan ramp lock, decide against the state that lock protects, and
// write, all before anyone else can read it.
//
// The lock is the point. The step range derives from the plan's CurrentStep and
// the "is this step already covered" test reads the executions of those steps,
// so both inputs are exactly the state a concurrent completion or a concurrent
// create mutates. Read either outside the lock and the check becomes advisory:
// two creates both see the step free and each mints a root row for it, and
// approving both re-fans-out over accounts that already bought. CompletePlanStep
// takes this same lock, so a completion cannot interleave either.
func (h *Handler) createPlannedPurchasesTx(ctx context.Context, tx pgx.Tx, planID string, count int, startDate time.Time, creator *string, session *Session) (int, error) {
	plan, err := h.config.LockPurchasePlanTx(ctx, tx, planID)
	if err != nil {
		logging.Errorf("createPlannedPurchases: LockPurchasePlanTx failed (plan=%s): %v", planID, err)
		return 0, NewClientError(503, "could not lock the plan for scheduling; try again")
	}
	if plan == nil {
		return 0, NewClientError(404, "plan not found")
	}
	if err := h.requirePlanMutationAccess(ctx, session, planID); err != nil {
		return 0, err
	}
	if plan.RampSchedule.DatesManaged {
		return 0, editorReconciliationError()
	}
	if err := h.validatePurchaseScheduling(ctx, plan, count, startDate); err != nil {
		return 0, err
	}

	if refuseErr := h.refuseOccupiedRampSteps(ctx, tx, plan, planID, count); refuseErr != nil {
		return 0, refuseErr
	}

	created, createErr := h.createPurchaseExecutionsTx(ctx, tx, plan, planID, count, startDate, creator)
	if createErr != nil {
		return 0, createErr
	}
	if planErr := h.updatePlanNextExecutionDateTx(ctx, tx, plan, startDate); planErr != nil {
		return 0, planErr
	}
	return created, nil
}

func (h *Handler) validatePurchaseScheduling(ctx context.Context, plan *config.PurchasePlan, count int, startDate time.Time) error {
	if !plan.Enabled {
		return NewClientError(409, "plan must be enabled before purchases can be scheduled")
	}
	if err := validateScheduledRamp(plan, count); err != nil {
		return err
	}
	accounts, err := h.config.GetPlanAccounts(ctx, plan.ID)
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		return NewClientError(409, "plan must have target accounts before purchases can be scheduled")
	}
	if plan.AutoPurchase {
		if err := h.requireAutomaticScheduling(ctx, plan); err != nil {
			return err
		}
	}
	// start_date is a date-only value parsed as midnight UTC, so today's
	// date is already in the past and is refused along with any earlier
	// day: tomorrow (UTC) is the first accepted date. The check runs here,
	// under the plan lock, because a request can wait on that lock past
	// UTC midnight (#873, #50).
	if !startDate.After(h.clock()) {
		return NewClientError(400, "start_date must be in the future")
	}
	return nil
}

// clock returns the handler's current time: the injected now func when a
// test set one, time.Now otherwise.
func (h *Handler) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

func validateScheduledRamp(plan *config.PurchasePlan, count int) error {
	if err := plan.Validate(); err != nil {
		return NewClientError(409, fmt.Sprintf("plan cannot be scheduled: %v", err))
	}
	if err := planstep.Precheck(plan); err != nil {
		return NewClientError(409, err.Error())
	}
	if err := plan.RequireVetoWindow(); err != nil {
		return NewClientError(409, err.Error())
	}
	ramp := plan.RampSchedule
	if count > ramp.TotalSteps-ramp.CurrentStep || (ramp.Type == "immediate" && ramp.TotalSteps != 1) {
		return NewClientError(409, "count exceeds the plan's remaining ramp steps")
	}
	if ramp.Type != "immediate" && ramp.StepIntervalDays <= 0 {
		return NewClientError(409, "step interval must be positive for scheduled ramp purchases")
	}
	return nil
}

func (h *Handler) requireAutomaticScheduling(ctx context.Context, plan *config.PurchasePlan) error {
	if plan.MaxStepCommitment == nil {
		return NewClientError(409, "automatic plans require max_step_commitment")
	}
	global, err := h.config.GetGlobalConfig(ctx)
	if err != nil {
		return err
	}
	if global == nil || !global.AutoPurchaseEnabled {
		return NewClientError(409, "automatic purchasing is disabled globally")
	}
	return nil
}

// refuseOccupiedRampSteps blocks a create whose steps overlap one that is
// already covered (issue #1861). Must be called with the plan's ramp lock held
// for the same transaction, which createPlannedPurchasesTx does.
//
// createPurchaseExecutionsTx stamps CurrentStep+1 .. CurrentStep+count. The
// completeness gate holds CurrentStep still while any account of a step is
// outstanding, so on a plan an operator is trying to unstick, CurrentStep+1 is
// the very step that is partly bought. The row minted for it is a ROOT row:
// approving it re-fans-out across every account on the plan, including the ones
// that already bought, under a fresh idempotency lineage (the new root's key is
// a new UUID, and each account's token derives from it), so the provider-side
// dedupe never engages and the commitment is genuinely bought twice. Per-account
// retry of the outstanding rows is the operation that actually finishes the step.
//
// Fails closed: an unreadable check refuses the create rather than minting rows
// that might double-buy.
func (h *Handler) refuseOccupiedRampSteps(ctx context.Context, tx pgx.Tx, plan *config.PurchasePlan, planID string, count int) error {
	from := plan.RampSchedule.CurrentStep + 1
	occupied, err := h.config.OccupiedRampStepsInRangeTx(ctx, tx, planID, from, from+count-1)
	if err != nil {
		logging.Errorf("createPlannedPurchases: OccupiedRampStepsInRangeTx failed (plan=%s): %v", planID, err)
		return NewClientError(503, "could not verify which ramp steps are already covered; try again")
	}
	if len(occupied) == 0 {
		return nil
	}
	return NewClientError(409, fmt.Sprintf(
		"ramp step(s) %s of this plan already have purchase executions that bought or are still in flight; "+
			"finish or cancel those instead of scheduling the same step again",
		formatRampSteps(occupied)))
}

// formatRampSteps renders step numbers for an operator-facing message.
func formatRampSteps(steps []int) string {
	out := ""
	for i, s := range steps {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%d", s)
	}
	return out
}

// parseCreatePurchasesRequest parses and validates the create purchases request.
func (h *Handler) parseCreatePurchasesRequest(body string) (*CreatePlannedPurchasesRequest, time.Time, error) {
	var req CreatePlannedPurchasesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		return nil, time.Time{}, NewClientError(400, "invalid request body")
	}

	if req.Count < 1 || req.Count > 52 {
		return nil, time.Time{}, NewClientError(400, "count must be between 1 and 52")
	}

	startDate, err := time.Parse("2006-01-02", req.StartDate)
	if err != nil {
		return nil, time.Time{}, NewClientError(400, "invalid start_date format, expected YYYY-MM-DD")
	}

	return &req, startDate, nil
}

// getPlanForPurchaseCreation retrieves and validates the purchase plan.
//
// Errors are routed through mapCreatePlanStorageError so a DB failure never
// leaks the raw error string through the router's
// logging.Errorf("API error: %v") path (issue #965, same shape as the
// validatePlanAccountProviders fix). The "plan not found" case (nil, nil) is
// returned as a 404 ClientError; planID is the caller-supplied value, so
// echoing it back in the 404 body is acceptable.
func (h *Handler) getPlanForPurchaseCreation(ctx context.Context, planID string) (*config.PurchasePlan, error) {
	plan, err := h.config.GetPurchasePlan(ctx, planID)
	if err != nil {
		return nil, mapCreatePlanStorageError(err,
			"plan not found", "failed to load plan",
			"getPlanForPurchaseCreation: GetPurchasePlan failed")
	}
	if plan == nil {
		return nil, NewClientError(404, fmt.Sprintf("plan not found: %s", planID))
	}
	return plan, nil
}

// createPurchaseExecutionsTx creates the per-row purchase executions
// inside the caller's transaction. A mid-loop failure rolls the whole
// transaction back, so the caller never sees orphaned rows on retry.
// Returns the number of rows that would have been committed had the
// loop completed — used for the user-visible response on success;
// undefined (and unused) on error since the rollback voids them all.
//
// creator carries the session user's UUID (or nil for the admin-API-key /
// non-UUID-session paths) and is stamped onto every inserted row's
// created_by_user_id so the issue-#950 ownership gate downstream can
// recognize the actor as the rightful manager. A nil value mirrors the
// migration-000041 fail-closed semantics: legacy / unattributed rows are
// reachable only by admin / update-any holders.
func (h *Handler) createPurchaseExecutionsTx(ctx context.Context, tx pgx.Tx, plan *config.PurchasePlan, planID string, count int, startDate time.Time, creator *string) (int, error) {
	intervalDays := plan.RampSchedule.StepIntervalDays

	created := 0
	for i := 0; i < count; i++ {
		scheduledDate := startDate.AddDate(0, 0, i*intervalDays)

		// No approval token (issue #103): nothing emails one from here, and
		// the notification job mints, emails and only then persists a token
		// for these future ramp steps (getOrCreateExecution). An empty token
		// fails closed at every compare site until then.
		execution := &config.PurchaseExecution{
			PlanID:          planID,
			ExecutionID:     uuid.New().String(),
			Status:          "pending",
			StepNumber:      plan.RampSchedule.CurrentStep + i + 1,
			ScheduledDate:   scheduledDate,
			CreatedByUserID: creator,
		}

		if err := h.config.SavePurchaseExecutionTx(ctx, tx, execution); err != nil {
			return created, fmt.Errorf("failed to save execution (row %d/%d): %w", created+1, count, err)
		}
		created++
	}

	return created, nil
}

// updatePlanNextExecutionDateTx bumps the plan's next_execution_date
// inside the caller's transaction so it commits atomically with the
// SavePurchaseExecutionTx writes from createPurchaseExecutionsTx.
func (h *Handler) updatePlanNextExecutionDateTx(ctx context.Context, tx pgx.Tx, plan *config.PurchasePlan, startDate time.Time) error {
	plan.RampSchedule.StartDate = startDate.AddDate(0, 0, -plan.RampSchedule.CurrentStep*plan.RampSchedule.StepIntervalDays)
	plan.NextExecutionDate = &startDate
	if err := h.config.UpdatePurchasePlanTx(ctx, tx, plan); err != nil {
		return fmt.Errorf("failed to update plan: %w", err)
	}
	return nil
}

// PatchPlanRequest represents a partial update request for plans.
type PatchPlanRequest struct {
	Name                   *string `json:"name,omitempty"`
	Enabled                *bool   `json:"enabled,omitempty"`
	AutoPurchase           *bool   `json:"auto_purchase,omitempty"`
	NotificationDaysBefore *int    `json:"notification_days_before,omitempty"`
}

// applyPatchFields applies validated partial-update fields to a plan.
func applyPatchFields(plan *config.PurchasePlan, req PatchPlanRequest) error {
	if req.Name != nil {
		if *req.Name == "" {
			return NewClientError(400, "plan name cannot be empty")
		}
		if len(*req.Name) > 255 {
			return NewClientError(400, "plan name too long (max 255 characters)")
		}
		plan.Name = *req.Name
	}
	if req.Enabled != nil {
		plan.Enabled = *req.Enabled
	}
	if req.AutoPurchase != nil {
		plan.AutoPurchase = *req.AutoPurchase
	}
	if req.NotificationDaysBefore != nil {
		if *req.NotificationDaysBefore < 0 || *req.NotificationDaysBefore > 30 {
			return NewClientError(400, "notification_days_before must be between 0 and 30")
		}
		plan.NotificationDaysBefore = *req.NotificationDaysBefore
	}
	return nil
}

// patchPlan handles partial updates to a plan (PATCH method).
func (h *Handler) patchPlan(ctx context.Context, httpReq *events.LambdaFunctionURLRequest, planID string) (any, error) {
	if err := validateUUID(planID); err != nil {
		return nil, err
	}

	session, err := h.requirePermission(ctx, httpReq, "update", "plans")
	if err != nil {
		return nil, err
	}

	err = h.requirePlanMutationAccess(ctx, session, planID)
	if err != nil {
		return nil, err
	}

	var req PatchPlanRequest
	err = json.Unmarshal([]byte(httpReq.Body), &req)
	if err != nil {
		return nil, NewClientError(400, "invalid request body")
	}

	// Do NOT wrap err with the raw plan UUID: a non-ClientError propagates to
	// the router's logging.Errorf("API error: %v"), leaking the plan UUID and
	// the raw DB error string (issue #965, same shape as the createPlan-side
	// fix).
	plan, err := h.config.GetPurchasePlan(ctx, planID)
	if err != nil {
		return nil, mapCreatePlanStorageError(err,
			"plan not found", "failed to load plan",
			"patchPlan: GetPurchasePlan failed")
	}
	if plan == nil {
		return nil, NewClientError(404, fmt.Sprintf("plan not found: %s", planID))
	}

	before := *plan
	if err := applyPatchFields(plan, req); err != nil {
		return nil, err
	}

	if err := h.validateAndGatePlan(ctx, httpReq, session, &before, plan); err != nil {
		return nil, err
	}

	if err := h.config.UpdatePurchasePlan(ctx, plan); err != nil {
		return nil, mapCreatePlanStorageError(err, "plan not found", "failed to update plan", "patchPlan: UpdatePurchasePlan failed")
	}

	return plan, nil
}
