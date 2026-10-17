package config

import "errors"

var ErrAzurePricingNotReady = errors.New("azure pricing migration not ready")

// ErrNotFound is returned when a requested config-store row does not exist.
var ErrNotFound = errors.New("not found")

// ErrEmptyPurchaseID is returned by SavePurchaseHistory for a record with no
// purchase_id. History is saved once per (provider, account_id, purchase_id),
// so an empty ID would be silently dropped after the first one (#774).
var ErrEmptyPurchaseID = errors.New("purchase history record has no purchase id")

// ErrPurchasePlanConflict means a plan changed after the caller read it.
var ErrPurchasePlanConflict = errors.New("purchase plan changed")

// ErrEditorReconciliationRequired means the plan is editor-managed and a legacy
// writer tried to change what only the schedule editor may. Permanent for the
// request: callers map it to 409; a background caller would skip, not retry.
var ErrEditorReconciliationRequired = errors.New("plan is managed by the schedule editor")

// ErrManagedPlanLiveExecutions means deleting an editor-managed plan would
// detach an execution that may still hold or buy capacity.
var ErrManagedPlanLiveExecutions = errors.New("managed plan still has active or unsettled purchases")

// ErrExecutionNotInExpectedStatus is returned by TransitionExecutionStatus
// when the target execution exists but its current status is not in the
// allowed `fromStatuses` set -- i.e. the atomic CAS rejected because some
// other writer transitioned the row first (e.g. the real executor finished
// between the reaper's SELECT and CAS). Callers can use errors.Is to
// distinguish this legitimate race-loss from a hard DB error.
var ErrExecutionNotInExpectedStatus = errors.New("execution not in expected status")

// ErrApprovalTokenChanged is returned (wrapped) by
// TransitionExecutionStatusTokenPinned when the execution is still in an
// allowed status but its stored approval token no longer matches the one the
// caller supplied, or has expired: the token was rotated (re-notification) or
// lapsed after the caller validated it and before the transition (issue #590).
var ErrApprovalTokenChanged = errors.New("approval token no longer matches the execution")

// ErrRampStepIncomplete is returned (wrapped) by CompletePlanStep when the
// ramp step the completing execution belongs to has not been bought in full:
// another cloud account on that step is still outstanding, or nothing on the
// step bought at all. Counting it would report commitment the plan has not
// made (issue #1861). Transient by construction -- it clears when the
// outstanding account buys, when its row is canceled, or when the step is
// abandoned -- so callers must treat it as "not yet", not as a failure of the
// purchase that just completed.
//
// The sentinel text stays generic because both cases wrap it and each supplies
// its own specifics; naming one of them here would mis-describe the other.
var ErrRampStepIncomplete = errors.New("ramp step incomplete")

// ErrRampStepCountedBySibling is returned (wrapped) by CompletePlanStep when
// the plan is sitting exactly on the completing step: a sibling execution of
// the same step won the race to advance it, moments earlier. Routine on a
// multi-account step whose last two accounts finish together, and benign --
// the step WAS counted, just not by this caller -- so it must not be stamped on
// the execution row. A note there would flip a cleanly-completed purchase into
// History's audit-gap rendering, which keys on a non-empty error.
var ErrRampStepCountedBySibling = errors.New("ramp step already counted by a sibling execution")

// ErrRampStepAlreadyCounted is returned (wrapped) by CompletePlanStep when the
// plan has moved PAST the completing step. Unlike ErrRampStepCountedBySibling
// this is an anomaly worth an audit note on the execution row: the purchase
// bought commitment for a step the ramp counted earlier and will never count
// again, which is how a step_number stamped by pre-#1669 code (or during that
// deploy overlap) surfaces.
var ErrRampStepAlreadyCounted = errors.New("ramp step already counted")

// ErrAuditLoss is returned (wrapped) by executeAndFinalize when the purchase
// run itself completed but the subsequent SavePurchaseExecution call failed.
// The execution is already "running" (per the CAS in claimAndRedrive) but its
// final state was never persisted -- the row is stranded in "running" until
// the next recovery sweep. Callers that silence all drive errors (e.g.
// claimAndRedrive) must propagate this sentinel so the sweep surfaces the
// persistence failure rather than silently dropping the stranded row.
var ErrAuditLoss = errors.New("audit loss: execution persistence failed after purchase")
