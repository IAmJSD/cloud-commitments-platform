package config

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
)

// StoreInterface defines the methods required for configuration storage.
type StoreInterface interface {
	// Global configuration
	GetGlobalConfig(ctx context.Context) (*GlobalConfig, error)
	SaveGlobalConfig(ctx context.Context, config *GlobalConfig) error
	// UpdateGlobalConfigAtomic serializes a read-modify-write of the
	// global_config singleton under an advisory-locked transaction so
	// concurrent partial PUTs cannot lose each other's updates. apply mutates
	// the loaded config in place; its error aborts the write and is propagated.
	// Each default_* value apply changes is copied onto every service config
	// in the same transaction.
	UpdateGlobalConfigAtomic(ctx context.Context, apply func(*GlobalConfig) error) (*GlobalConfig, error)

	// Service configuration
	GetServiceConfig(ctx context.Context, provider, service string) (*ServiceConfig, error)
	SaveServiceConfig(ctx context.Context, config *ServiceConfig) error
	// UpdateServiceConfigAtomic serializes a read-merge-write of one service
	// config row under a locked transaction so concurrent partial PUTs of the
	// same row cannot lose each other's updates. apply receives the stored row
	// (nil when none exists) and returns the full row to write; its error
	// aborts the write and is propagated.
	UpdateServiceConfigAtomic(ctx context.Context, provider, service string, apply func(existing *ServiceConfig) (*ServiceConfig, error)) (*ServiceConfig, error)
	ListServiceConfigs(ctx context.Context) ([]ServiceConfig, error)

	// Purchase plans
	CreatePurchasePlan(ctx context.Context, plan *PurchasePlan) error
	GetPurchasePlan(ctx context.Context, planID string) (*PurchasePlan, error)
	// UpdatedAt must be the read version; a stale version returns ErrPurchasePlanConflict.
	UpdatePurchasePlan(ctx context.Context, plan *PurchasePlan) error
	StampPlanNotificationSent(ctx context.Context, planID string, at time.Time) error
	// CompletePlanStep records that ramp step stepNumber finished and advances
	// the plan's schedule to it, inside a SELECT FOR UPDATE transaction that
	// prevents the concurrent-write lost-update race of issue #1071.
	//
	// It is idempotent in stepNumber: completing a step the plan has already
	// counted writes nothing. Which sentinel says so depends on where the ramp
	// sits relative to the completing step, and callers branch on the
	// difference (purchase.recordRampAdvanceRefusal does):
	//
	//   - CurrentStep == stepNumber reports ErrRampStepCountedBySibling. This
	//     is the issue #1669 scenario: an operator retries two separately-failed
	//     accounts of one ramp step, and the second completion finds the ramp
	//     already sitting on that step because the first advanced it. The step
	//     WAS counted, so this is routine and is not recorded against the
	//     execution.
	//   - CurrentStep > stepNumber reports ErrRampStepAlreadyCounted. The ramp
	//     moved past the step earlier, so this purchase bought commitment the
	//     plan will never count. That is an anomaly and IS recorded.
	//
	// A step counts as complete only when EVERY cloud account the step fanned
	// out to has bought, not when any one of them has (issue #1861). Until
	// then it reports ErrRampStepIncomplete, which is a "not yet" rather than a
	// failure of the purchase that just completed -- the purchase is real and
	// already recorded; only the plan's position is withheld.
	CompletePlanStep(ctx context.Context, planID string, stepNumber int) error
	// GetStuckRampSteps returns, keyed by plan ID, the plan's next ramp step
	// and how many of that step's executions have a terminal, unsuccessful
	// latest attempt with no retry in flight -- a ramp CompletePlanStep will
	// keep refusing to advance until an operator intervenes. Plans with
	// nothing stuck are absent from the map. Feeds the plan-health
	// ramp_blocked factor (issue #1861).
	GetStuckRampSteps(ctx context.Context) (map[string]RampStepBlock, error)
	// LockPurchasePlanTx reads a plan under a row lock held for the rest of tx.
	// It is the per-plan ramp lock CompletePlanStep takes, exposed so a caller
	// that reads a plan's ramp position and then writes against it does so
	// atomically with respect to concurrent completions and creations.
	// Returns (nil, nil) when the plan does not exist.
	LockPurchasePlanTx(ctx context.Context, tx pgx.Tx, planID string) (*PurchasePlan, error)
	// LockPlanProvidersTx takes the transaction-scoped advisory lock of every
	// named provider, in sorted order, so two plan steps that resolve the same
	// provider's recommendations cannot both attach the same capacity. Lock
	// order is advisory lock, then LockPurchasePlanTx. The ladder config lock
	// (PlanAndSaveLadderRun, ClaimDueLadderTranches, AmendLadderTranche) is always taken BEFORE both:
	// ClaimDueLadderTranches's create callback may take these locks, so they
	// come after the ladder lock; code holding an execution or tranche row
	// (retry linker, reconciler) must never take the ladder lock. No cycle.
	// Duplicates are taken once; an empty list and an empty provider name are
	// errors.
	LockPlanProvidersTx(ctx context.Context, tx pgx.Tx, providers []string) error
	// OccupiedRampStepsInRangeTx returns the steps in [from, to] of planID that
	// already have a fan-out unit which bought or is still working, ascending.
	// Callers about to mint executions for a step range use it to refuse a step
	// that is already covered, which would otherwise re-fan-out across accounts
	// that already committed under a fresh idempotency lineage (issue #1861).
	// Requires the caller to hold LockPurchasePlanTx on planID in the same
	// transaction; without it the answer is advisory and two concurrent callers
	// both see the step free.
	OccupiedRampStepsInRangeTx(ctx context.Context, tx pgx.Tx, planID string, from, to int) ([]int, error)
	// ListExecutionsForPlanStepTx returns EVERY execution row (any status,
	// account or retry attempt) recorded for stepNumber of planID. The
	// get-or-create of a plan step reads it under LockPurchasePlanTx so any
	// existing row, settled or not, blocks minting a second root (platform#631).
	ListExecutionsForPlanStepTx(ctx context.Context, tx pgx.Tx, planID string, stepNumber int) ([]PurchaseExecution, error)
	// SetPlanRampBaselineTx freezes the plan's ramp baseline if none is set and
	// reports whether it wrote. Immutable once set; not part of
	// UpdatePurchasePlanTx, so a stale plan save cannot touch it. The caller
	// holds LockPurchasePlanTx.
	SetPlanRampBaselineTx(ctx context.Context, tx pgx.Tx, planID string, baseline PlanRampBaseline) (bool, error)
	// GetPlanRampBaselineTx returns the frozen baseline, nil when none is set.
	GetPlanRampBaselineTx(ctx context.Context, tx pgx.Tx, planID string) (PlanRampBaseline, error)
	// ListPlanCommittedTx returns what the plan's executions already hold per
	// recommendation identity (see its implementation for the status rules).
	// The caller holds LockPurchasePlanTx.
	ListPlanCommittedTx(ctx context.Context, tx pgx.Tx, planID string) (map[string]PlanRampQuantity, error)
	// CompleteTargetMetStepTx records a plan step whose cumulative target is
	// already held as a completed no-op row (new, or rootID's empty root row
	// completed in place) and advances the ramp over it in the same
	// transaction. The caller holds LockPurchasePlanTx.
	CompleteTargetMetStepTx(ctx context.Context, tx pgx.Tx, planID string, stepNumber int, rootID, note string) error
	// SetExecutionRecommendationsIfEmptyTx attaches resolved recommendations
	// and their totals to a pending/notified execution that has none yet.
	// Returns false when another resolver already attached them (or the row
	// left pending/notified); the caller re-reads instead of overwriting.
	SetExecutionRecommendationsIfEmptyTx(ctx context.Context, tx pgx.Tx, executionID string, recs []RecommendationRecord, upfront, savings float64) (bool, error)
	// ExpireExecutionAtomic flips a pending/notified execution to expired in
	// tx so its suppressions can be deleted in the same transaction. Returns
	// false when the row had already left pending/notified.
	ExpireExecutionAtomic(ctx context.Context, tx pgx.Tx, executionID string) (bool, error)
	// UpdatePurchasePlanTx checks UpdatedAt and replaces it with the returned version.
	// The caller must discard the transaction-local plan if the transaction rolls back.
	UpdatePurchasePlanTx(ctx context.Context, tx pgx.Tx, plan *PurchasePlan) error
	DeletePurchasePlan(ctx context.Context, planID string) error
	ListPurchasePlans(ctx context.Context, filter PurchasePlanFilter) ([]PurchasePlan, error)

	// Purchase executions
	SavePurchaseExecution(ctx context.Context, execution *PurchaseExecution) error
	// SaveExecutionIfStatus saves only while the stored status is one of
	// expected (ErrExecutionNotInExpectedStatus otherwise), so a stale
	// in-memory copy cannot rewrite a status another writer moved on.
	SaveExecutionIfStatus(ctx context.Context, execution *PurchaseExecution, expected []string) error
	// RecoverReapedExecution persists a late completed/partially_completed
	// outcome over a reaper-failed row that has no retry successor linked
	// (ErrExecutionRetryLinked when one is).
	RecoverReapedExecution(ctx context.Context, execution *PurchaseExecution) error
	// PinExecutionFanOutShape records shape (FanOutShapeSingle or
	// FanOutShapeMulti) for the execution lineage lineageKey unless one is
	// already recorded, and returns the recorded shape (issue #97).
	PinExecutionFanOutShape(ctx context.Context, lineageKey, shape string) (string, error)
	// GetExecutionFanOutShape returns the shape pinned for lineageKey, or ""
	// when none is pinned yet (issue #97).
	GetExecutionFanOutShape(ctx context.Context, lineageKey string) (string, error)
	GetPendingExecutions(ctx context.Context) ([]PurchaseExecution, error)
	// GetExecutionsByStatuses returns executions in any of the given states,
	// newest first, capped at `limit`, except that pending rows dated in the
	// future (not-yet-due ramp steps) sort last, soonest first, so they cannot
	// push a due row past the cap (issue #680). Used by the History handler to render
	// pending + failed + expired alongside completed purchases; the scheduler
	// keeps using GetPendingExecutions (which is narrower and doesn't share
	// this method's status filter) to avoid accidental double-processing of
	// failed / expired rows.
	GetExecutionsByStatuses(ctx context.Context, statuses []string, limit int) ([]PurchaseExecution, error)
	// GetExecutionsByStatusesFiltered is GetExecutionsByStatuses with the
	// History filters and account scope in q applied in SQL before the limit,
	// so the cap counts only rows the caller can be shown (issue #632).
	GetExecutionsByStatusesFiltered(ctx context.Context, q ExecutionsByStatusesQuery) ([]PurchaseExecution, error)
	// ListHistoryExecutionsPage and ListPurchaseHistoryPage read one keyset
	// page each of the History view's paged stream (issue #674): rows strictly
	// after q.After / after (nil starts at the top) in the stream order of
	// HistoryPageKey, capped at the limit. The executions half excludes clean
	// completed rows like GetExecutionsByStatuses and bounds scheduled_date
	// inclusively; the purchase_history half applies GetPurchaseHistoryFiltered's
	// predicate. Rows inserted or changed between two page reads never shift
	// the rows a later page returns.
	ListHistoryExecutionsPage(ctx context.Context, q HistoryExecutionsPageQuery) ([]PurchaseExecution, error)
	ListPurchaseHistoryPage(ctx context.Context, filter PurchaseHistoryFilter, after *HistoryPageKey) ([]PurchaseHistoryPageRow, error)
	// CountExecutionsByPlanAndStatus returns, keyed by plan ID, the exact
	// number of executions in each of the supplied statuses that last
	// changed state (updated_at) at or after `since` -- not those merely
	// scheduled since then, which for a canceled row is a future date. Rows
	// with a NULL plan_id (direct-execute purchases, and executions orphaned
	// by a deleted plan) belong to no plan and are excluded.
	//
	// Deliberately NOT derived from a GetExecutionsByStatuses page: that
	// method is capped by `limit` across ALL plans, so any caller counting
	// per-plan rows out of it silently understates plans whose executions
	// fall outside the newest `limit` rows. The plan health score
	// (internal/api/plan_health.go) needs exact counts -- an understated
	// count renders a confidently-healthy badge for an unhealthy plan --
	// so the aggregation happens in SQL and the result is bounded by
	// plans x statuses rather than by execution volume.
	CountExecutionsByPlanAndStatus(ctx context.Context, statuses []string, since time.Time) (map[string]ExecutionStatusCounts, error)
	// GetPlannedExecutions returns executions in any of the given states
	// ordered by scheduled_date ASC (soonest first), the order the Planned
	// Purchases UI lists rows so the user acts on imminent purchases first.
	// Distinct from GetExecutionsByStatuses (which is DESC for History's
	// "newest first" semantics): when the result set exceeds `limit`, an
	// ORDER-BY-DESC + LIMIT in SQL truncates away the soonest rows, exactly
	// the rows this list must surface. Secondary sort by id ASC stabilizes
	// ordering when multiple rows share a scheduled_date.
	GetPlannedExecutions(ctx context.Context, statuses []string, limit int) ([]PurchaseExecution, error)
	// GetStaleApprovedExecutions returns executions stuck in the "approved"
	// status with updated_at older than olderThan — strands left behind when a
	// synchronous purchase run was interrupted before finalizing (issue #632).
	// The recovery sweep in the purchase manager re-drives these into a
	// terminal "failed" state so they can never sit permanently approved.
	GetStaleApprovedExecutions(ctx context.Context, olderThan time.Duration) ([]PurchaseExecution, error)
	// GetExecutionByID retrieves a purchase execution by execution ID.
	// Returns an error wrapping ErrNotFound when no execution exists;
	// never returns (nil, nil). A nil error guarantees a non-nil execution.
	GetExecutionByID(ctx context.Context, executionID string) (*PurchaseExecution, error)
	GetExecutionByPlanAndDate(ctx context.Context, planID string, scheduledDate time.Time) (*PurchaseExecution, error)
	// GetUserEmailByID resolves the email address of the auth user identified
	// by userID (the `users` table, owned by internal/auth). Read-only helper
	// used by the 4-eyes approval policy (issue #1005) to compare the acting
	// approver's identity against a PurchaseExecution.CreatedByUserID without
	// internal/purchase importing internal/auth (which would create an import
	// cycle: internal/auth already imports internal/config). Returns ("", nil)
	// when no user matches userID — the caller treats an empty result as
	// "identity unresolved" and fails closed, never as "email intentionally
	// blank".
	GetUserEmailByID(ctx context.Context, userID string) (string, error)
	// CountPendingExecutionsForAccount returns the number of purchase_executions
	// in status 'pending' or 'notified' that reference the given cloud account.
	// Used by the deleteAccount handler to preflight DB-level FK violations
	// (migration 000053 tightened the FK to ON DELETE RESTRICT) and emit a
	// 409 with a count so the frontend can offer Cancel-All-Then-Delete UX
	// instead of surfacing a raw constraint error. See issue #606.
	CountPendingExecutionsForAccount(ctx context.Context, accountID string) (int, error)
	// CountLiveExecutionsForAccount returns the number of purchase_executions
	// referencing the given cloud account whose status is not in
	// TerminalExecutionStatuses (pending, notified, approved, running,
	// scheduled, paused, or any status not yet classified). These are the
	// rows that block DeleteCloudAccount; terminal rows are detached and kept
	// as history instead (issue #265).
	CountLiveExecutionsForAccount(ctx context.Context, accountID string) (int, error)
	// ListPendingExecutionIDsForAccount returns the execution IDs of all
	// pending / notified executions referencing this account, used by the
	// frontend's Cancel-All-Then-Delete flow when the operator opts to
	// cancel everything in one go after a 409. Capped at 1000 rows; if a
	// single account has more pending executions than that, the cleanup
	// is a one-off operator task rather than a button click anyway.
	ListPendingExecutionIDsForAccount(ctx context.Context, accountID string) ([]string, error)
	CleanupOldExecutions(ctx context.Context, retentionDays int) (int64, error)
	// TransitionExecutionStatus atomically transitions an execution status.
	// actor is the UUID of the user performing the transition (nil for system-initiated paths).
	// When non-nil the actor is stamped onto transitioned_by + transitioned_at; when nil,
	// transitioned_by is set to NULL and transitioned_at is still set to NOW() for ordering.
	TransitionExecutionStatus(ctx context.Context, executionID string, fromStatuses []string, toStatus string, actor *string) (*PurchaseExecution, error)
	// TransitionExecutionStatusPinned is TransitionExecutionStatus that also
	// requires the row's recommendations to be empty (sawEmptyRecommendations)
	// or non-empty exactly as the caller saw them, so a claim cannot land on a
	// row whose recommendations were attached after the caller's gate check. A
	// lost pin returns ErrExecutionNotInExpectedStatus like any lost CAS.
	TransitionExecutionStatusPinned(ctx context.Context, executionID string, fromStatuses []string, toStatus string, actor *string, sawEmptyRecommendations bool) (*PurchaseExecution, error)
	// TransitionExecutionStatusPinnedTx is TransitionExecutionStatusPinned
	// inside a caller-owned transaction, so the status change commits or
	// rolls back with the caller's other writes (issue #1052).
	TransitionExecutionStatusPinnedTx(ctx context.Context, tx pgx.Tx, executionID string, fromStatuses []string, toStatus string, actor *string, sawEmptyRecommendations bool) (*PurchaseExecution, error)
	// TransitionExecutionStatusTokenPinned is TransitionExecutionStatus that
	// also requires the row's stored approval token hash to match rawToken and
	// to be unexpired, in the same statement as the status change, so a token
	// rotated or expired after the caller validated it cannot win the
	// transition (issue #590). A lost token pin on a row still in an allowed
	// status returns ErrApprovalTokenChanged; a lost status CAS returns
	// ErrExecutionNotInExpectedStatus as usual.
	TransitionExecutionStatusTokenPinned(ctx context.Context, executionID string, fromStatuses []string, toStatus string, actor *string, rawToken string) (*PurchaseExecution, error)
	// RescheduleAwaitingExecution sets scheduled_date on an execution that is
	// still pending or notified and touches no other column. It returns false
	// (and writes nothing) when the row has left those statuses, so a cancel or
	// approve that lands first is never overwritten.
	RescheduleAwaitingExecution(ctx context.Context, executionID string, scheduledDate time.Time) (bool, error)
	// SetCancelledBy stamps canceled_by and the legacy cancelled_by column on
	// an execution without overwriting any other columns. Used after
	// TransitionExecutionStatus to fold the actor attribution into the same
	// logical write without the full-row SavePurchaseExecution clobber risk
	// (Finding #5 / PR #889).
	SetCancelledBy(ctx context.Context, executionID string, cancelledBy string) error
	// LinkRetryExecutionAtomic stamps retry_execution_id on a failed
	// execution row without a full-row overwrite, conditional on
	// status = 'failed' AND retry_execution_id IS NULL. Replaces a
	// SavePurchaseExecutionTx upsert of a stale in-memory copy of the
	// failed row, which let two concurrent retries of the same row both
	// pass the in-memory already-retried guard and both persist an
	// approvable successor (issue #220). Returns (true, nil) when this
	// call won the race, (false, nil) when a concurrent retry already
	// claimed the row (or it left 'failed'), and (false, err) on a real
	// DB error. Must be called inside the same tx that inserts the
	// successor row, after that insert.
	LinkRetryExecutionAtomic(ctx context.Context, tx pgx.Tx, executionID, retryExecutionID string) (linked bool, err error)
	// LockLadderTrancheForRetryTx row-locks the ladder tranche whose id equals
	// idempotencyKey (matched as text; found=false for any non-ladder key) so
	// Retry can refuse a settled tranche before inserting a successor.
	LockLadderTrancheForRetryTx(ctx context.Context, tx pgx.Tx, idempotencyKey string) (status ladder.TrancheStatus, found bool, err error)
	// GetLadderExecutionGate returns the ladder config switches and run status
	// behind the tranche whose id equals idempotencyKey (matched as text), or
	// (nil, nil) for any non-ladder key. The executor rechecks it before a
	// provider call and fails closed on error.
	GetLadderExecutionGate(ctx context.Context, idempotencyKey string) (*LadderExecutionGate, error)
	// RotatePendingApprovalToken sets approval_token_hash and its expiry only
	// while the row is still pending/notified, touching no other column, so a
	// re-notification can never clobber a concurrent approve/cancel the way a
	// full-row upsert of a stale copy would. Returns false when the row left
	// pending/notified (issue #103).
	RotatePendingApprovalToken(ctx context.Context, executionID, tokenHash string, expiresAt time.Time) (rotated bool, err error)
	// CancelExecutionAtomic atomically flips status from pending / notified
	// to 'canceled' (canonical US spelling), setting canceled_by. The
	// 'scheduled' status is NOT accepted here; scheduled rows are revoked via
	// CancelScheduledExecutionAtomic (Gmail-style pre-fire delay revoke
	// path, issue #291 wave-2) so the two flows surface distinct CAS race
	// outcomes. Returns (true, "canceled", nil) on success and (false,
	// currentStatus, nil) when zero rows were affected (the execution had
	// already been approved or otherwise transitioned). Must be called
	// inside a WithTx block so the suppression cleanup and the status flip
	// commit atomically.
	CancelExecutionAtomic(ctx context.Context, tx pgx.Tx, executionID string, cancelledBy *string) (canceled bool, currentStatus string, err error)
	// CancelExecutionAtomicTokenPinned is CancelExecutionAtomic that also
	// requires the row's stored approval token hash to match rawToken and to
	// be unexpired, in the same statement as the status change, so a token
	// rotated or expired after the email-link cancel validated it cannot
	// cancel the row (issue #1190). A lost token pin on a row still
	// pending/notified returns an error wrapping ErrApprovalTokenChanged; a
	// lost status CAS returns (false, currentStatus, nil) as usual.
	CancelExecutionAtomicTokenPinned(ctx context.Context, tx pgx.Tx, executionID string, cancelledBy *string, rawToken string) (canceled bool, currentStatus string, err error)
	// CancelScheduledExecutionAtomic atomically flips status from 'scheduled' to
	// 'canceled' (canonical US spelling), setting canceled_by. Used by the
	// Gmail-style pre-fire delay revoke path (issue #291 wave-2) to cancel a
	// scheduled execution at $0 before the scheduler fires the SDK call. The
	// 'pending'/'notified' set accepted by CancelExecutionAtomic is
	// intentionally not extended here so the two revoke flows surface distinct
	// CAS race outcomes -- a scheduled row that the scheduler has already
	// transitioned to 'approved' / 'running' must surface as a 410 ("window
	// closed") rather than a 409 ("not pending"). Returns (true, "canceled",
	// nil) on success and (false, currentStatus, nil) when zero rows were
	// affected. Must be called inside a WithTx block.
	CancelScheduledExecutionAtomic(ctx context.Context, tx pgx.Tx, executionID string, cancelledBy *string) (canceled bool, currentStatus string, err error)
	// ListStuckExecutions returns executions in any of the given statuses
	// whose updated_at is older than the given duration. Used by the
	// reaper sweep (issue #678) to find rows stuck in approved/running
	// after the synchronous executor failed mid-flight without flipping
	// them to a terminal state. Oldest-stuck-first (ORDER BY updated_at
	// ASC), capped at MaxListLimit per sweep.
	ListStuckExecutions(ctx context.Context, statuses []string, olderThan time.Duration) ([]PurchaseExecution, error)

	// GetScheduledExecutionsDue returns purchase_executions with
	// status='scheduled' whose scheduled_execution_at is in the past
	// (scheduled_execution_at <= NOW()). Used by the Gmail-style pre-fire
	// delay scheduler tick (issue #291 wave-2) to find rows ready to fire.
	// Oldest-due-first (ORDER BY scheduled_execution_at ASC), capped at
	// MaxListLimit per sweep.
	GetScheduledExecutionsDue(ctx context.Context) ([]PurchaseExecution, error)

	// Purchase history
	SavePurchaseHistory(ctx context.Context, record *PurchaseHistoryRecord) error
	GetPurchaseHistory(ctx context.Context, accountID string, limit int) ([]PurchaseHistoryRecord, error)
	GetAllPurchaseHistory(ctx context.Context, limit int) ([]PurchaseHistoryRecord, error)
	// GetActivePurchaseHistory returns every purchase_history row whose commitment
	// is still within its term at asOf (term > 0 AND timestamp + term years >= asOf;
	// the expiry boundary is inclusive, matching the API layer's isActiveCommitment),
	// newest-first, optionally scoped to a set of accounts. The account scope uses
	// the same dual-column predicate as GetPurchaseHistoryFiltered: accountIDs
	// match cloud_account_id (cloud_accounts UUIDs) and externalIDsByProvider
	// matches account_id per provider, OR'd together so rows carrying only one
	// identifier are still returned (issues #701/#498/#866). Both empty means all
	// accounts. Unlike GetAllPurchaseHistory it is not row-capped: the analytics
	// collector, dashboard KPIs, and inventory endpoints need the complete active
	// set, and filtering expired commitments in SQL keeps the result bounded by
	// the number of live commitments rather than by all history ever recorded (so
	// it cannot silently truncate older-but-still-active 1y/3y commitments the
	// way a newest-first capped page does, issue #1140).
	GetActivePurchaseHistory(ctx context.Context, asOf time.Time, accountIDs []string, externalIDsByProvider map[string][]string) ([]PurchaseHistoryRecord, error)
	// GetPurchaseHistoryFiltered reads purchase_history rows matching the
	// PurchaseHistoryFilter, newest-first, capped at filter.Limit. Each field is
	// applied independently and only when populated (see PurchaseHistoryFilter).
	// The account predicate matches BOTH identifier columns:
	//   (cloud_account_id = ANY(AccountIDs)
	//      OR ((provider = '' OR LOWER(provider) = LOWER($p))
	//          AND account_id = ANY(ExternalIDsByProvider[p])) OR ...)
	// because purchase_history rows carry the cloud_accounts UUID FK
	// (cloud_account_id, NULL on direct-execute/ambient/pre-000011 rows) and the
	// cloud-provider external number (account_id, always populated) independently.
	// The top-bar Account chip emits the UUID; matching only one column silently
	// dropped rows that carried only the other (issues #701/#498/#866). The caller
	// resolves AccountIDs to their external account numbers grouped by provider
	// and populates ExternalIDsByProvider so rows that carry only account_id are
	// also matched, while the per-provider grouping keeps a reused external number
	// across providers (aws/123 vs azure/123) from leaking the wrong rows. The
	// provider gate is case-insensitive and a row with an empty (unknown) stored
	// provider matches on account_id alone (issue #24).
	GetPurchaseHistoryFiltered(ctx context.Context, filter PurchaseHistoryFilter) ([]PurchaseHistoryRecord, error)
	// HistorySummaryAggregate returns counts and dollar sums over every row the
	// History view would list without its row caps: purchase_history rows
	// matched with GetPurchaseHistoryFiltered's predicate and purchase_executions
	// rows matched with GetExecutionsByStatuses' predicate (issue #675). Rows are
	// grouped by the columns the History handler's account-scope check reads, so
	// the caller applies that check to the groups with the same code it applies
	// to fetched rows. Both halves are read from one snapshot.
	HistorySummaryAggregate(ctx context.Context, filter HistorySummaryFilter) (*HistorySummaryGroups, error)
	// GetPurchaseHistoryByPurchaseID returns the single purchase_history row
	// whose purchase_id matches (AWS ReservedInstancesId / Azure reservation
	// ID). Returns (nil, nil) when no row is found. Used by the revoke
	// endpoint to load the record before calling the provider cancel API
	// (issue #290) and by the marketplace-list handler to validate
	// offering_class and look up the cloud account (issue #292).
	GetPurchaseHistoryByPurchaseID(ctx context.Context, purchaseID string) (*PurchaseHistoryRecord, error)
	// MarkPurchaseRevoked stamps revoked_at, revoked_via, and optionally
	// support_case_id on a purchase_history row identified by purchase_id.
	// calcRefundAmount and calcRefundCurrency capture the Azure CalculateRefund
	// quote for audit (migration 000071, Finding #4); both nil/empty for
	// non-Azure paths or legacy rows written before the migration. Also
	// clears revocation_in_flight (migration 000072): a revoked row is no
	// longer in flight.
	// Returns a not-found error when no row matches. Idempotent: a second
	// call for the same row is a no-op (revoked_at is not overwritten when
	// it is already non-null). Used by the revoke endpoint (issue #290).
	MarkPurchaseRevoked(ctx context.Context, purchaseID string, revokedAt time.Time, revokedVia string, supportCaseID string, calcRefundAmount *float64, calcRefundCurrency string) error

	// FlipPurchaseRevocationInFlight atomically sets revocation_in_flight=true
	// on a purchase_history row. Called immediately before the Azure Return API
	// call so that the row can be identified by the finalize sweep if the
	// subsequent MarkPurchaseRevoked DB write fails (partial-success reconciliation,
	// issue #290 Finding #6, migration 000072). No-op when the flag is already
	// true (idempotent). Returns a not-found error when no row matches.
	FlipPurchaseRevocationInFlight(ctx context.Context, purchaseID string) error

	// ClearRevocationInFlight resets revocation_in_flight=false on a
	// purchase_history row. Called when the Azure Return call fails with a
	// transient or client error (not "already returned"), so the row is not
	// left in a permanently-sticky in-flight state that would prevent future
	// retries or mislead the finalize_revocations sweep (issue #290, second-wave
	// CR Finding D). No-op when the row is already false. Best-effort: callers
	// should log on error but not surface it to the user.
	ClearRevocationInFlight(ctx context.Context, purchaseID string) error

	// GetPurchaseHistoryInFlight returns all purchase_history rows with
	// revocation_in_flight=true and revoked_at IS NULL.  These are rows where
	// the Azure Return call succeeded but MarkPurchaseRevoked failed; the
	// finalize_revocations scheduled sweep calls this to retry the DB write.
	GetPurchaseHistoryInFlight(ctx context.Context) ([]*PurchaseHistoryRecord, error)

	// UpdatePurchaseHistoryListing stamps the AWS marketplace listing_id and
	// listing_state onto a purchase_history row. Called after
	// CreateReservedInstancesListing succeeds (listing_state="active") and
	// on subsequent poll/cancel transitions (issue #292). Recording a listing
	// resolves the attempt, so it also clears the persisted client token and
	// price schedule (issue #525). It never marks a listing the row records as
	// canceled or closed live again; that write returns
	// ErrMarketplaceListingEnded.
	UpdatePurchaseHistoryListing(ctx context.Context, purchaseID, listingID, listingState string) error

	// GetMarketplaceListingAttempt returns the listing id, state and the token
	// of an unresolved list attempt of the row for purchaseID, or (nil, nil)
	// when there is none. The cancel endpoint uses it to recover a pending row
	// (issue #525).
	GetMarketplaceListingAttempt(ctx context.Context, purchaseID string) (*MarketplaceListingAttempt, error)

	// ResolveMarketplaceListingAttempt records listingID in an ended (or, to
	// clear a stuck pending row without a listing, empty) state and drops the
	// attempt token, only while the row still matches seen or already holds
	// listingID. It reports whether the row was written (issue #525).
	ResolveMarketplaceListingAttempt(ctx context.Context, purchaseID string, seen MarketplaceListingAttempt, listingID, state string) (bool, error)

	// StampOfferingClass writes the offering_class value to a purchase_history
	// row identified by purchase_id. Called by the marketplace-list handler
	// when offering_class is absent in the DB (pre-migration 000087 rows and
	// externally-created Standard RIs): after fetching the class from AWS
	// DescribeReservedInstances it is persisted so subsequent requests do not
	// incur an extra AWS API call.
	StampOfferingClass(ctx context.Context, purchaseID, offeringClass string) error

	// ClaimMarketplaceListingSlot atomically reserves the marketplace-listing
	// slot for a purchase_history row so two concurrent marketplace-list
	// requests cannot both proceed to create a duplicate AWS listing (issue
	// #292). It transitions listing_state to ListingStatePending only when the
	// row is not listed, and in the same UPDATE persists clientToken and
	// priceSchedule (JSON) unless an unresolved attempt already stored a token,
	// in which case that token and schedule are kept so the retry is idempotent
	// on AWS (issue #525). A pending row that holds a token is claimable again:
	// the same token makes a concurrent or crashed attempt safe to resume.
	// It returns the claimed row's values, or (nil, nil) when the row is active,
	// pending without a token, or absent (the caller maps that to a 409). The
	// caller must persist the real listing on success or call
	// ReleaseMarketplaceListingClaim on failure.
	ClaimMarketplaceListingSlot(ctx context.Context, purchaseID, clientToken string, priceSchedule []byte) (*MarketplaceListingClaim, error)

	// ReleaseMarketplaceListingClaim moves a pending row back to priorState
	// (empty for none) after a failed attempt. It only acts on a row whose
	// persisted token is clientToken, so a request never releases or drops the
	// attempt of another request. keepAttempt keeps the persisted token and
	// schedule so the retry resends them (ambiguous AWS outcome); otherwise
	// they are cleared because AWS created nothing reusable.
	ReleaseMarketplaceListingClaim(ctx context.Context, purchaseID, clientToken, priorState string, keepAttempt bool) error

	// ListMarketplaceListingsToPoll returns the AWS rows whose listing the
	// marketplace status poller reads (issue #27): active listings, listings
	// recorded as pending with no list attempt in flight (issue #1494), and
	// closed/canceled ones it has seen whose proceeds are not recorded yet.
	ListMarketplaceListingsToPoll(ctx context.Context) ([]PurchaseHistoryRecord, error)

	// RecordMarketplaceListingActive records a listing AWS reports active:
	// it stamps listed_at (a zero listedAt keeps the stored one) while the row
	// holds listingID as active, and also moves the state to active while the
	// row holds it as pending with no list attempt in flight (issue #1494).
	// It never writes the state of any other row. Reports whether the row
	// changed.
	RecordMarketplaceListingActive(ctx context.Context, purchaseID, listingID string, listedAt time.Time) (bool, error)

	// SettleMarketplaceListing records a listing that closed or was canceled
	// on AWS, with its proceeds and AWS fee when known. It applies once per
	// listing, only while the row still holds it and is active, pending with
	// no list attempt in flight, or already in the same terminal state.
	// Reports whether the row was written.
	SettleMarketplaceListing(ctx context.Context, settlement MarketplaceListingSettlement) (bool, error)

	// ClaimRIExchangeIdempotencyKey atomically claims key for an RI exchange
	// submit, so a client that retries a timed-out execute request cannot
	// commit the same exchange twice (issue #1642). key is a fingerprint of
	// what the request buys; the caller derives it and claims it immediately
	// before the irreversible provider call.
	//
	// Returns (true, nil) when this call won the claim and may proceed to
	// commit, and (false, nil) when another submit of the same fingerprint
	// claimed it less than window ago (the caller maps this to a 409).
	// A claim older than window is reclaimable, so a genuine intentional
	// repeat of the same exchange is not blocked forever.
	//
	// There is no release: once taken, a claim stands for the rest of the
	// window whatever the provider call does, including when it fails. Past
	// the point of submission the outcome is ambiguous, and holding the claim
	// is the fail-closed choice. The consequence for the caller's 409 is that
	// it cannot promise the earlier submit committed, only that it claimed the
	// window.
	ClaimRIExchangeIdempotencyKey(ctx context.Context, key string, window time.Duration) (bool, error)

	// RI Exchange history
	SaveRIExchangeRecord(ctx context.Context, record *RIExchangeRecord) error
	ReserveRIExchange(ctx context.Context, record *RIExchangeRecord, dailyCapUSD, perExchangeCapUSD string) (string, error)
	// ReserveApprovedRIExchange reserves the execution ceiling for an already
	// processing (approved) exchange under the shared daily-cap lock, excluding
	// the row's own amount, and stores the ceiling as its payment_due.
	ReserveApprovedRIExchange(ctx context.Context, id, dailyCapUSD, perExchangeCapUSD string) (string, error)
	// GetRIExchangeRecord returns a wrapped ErrNotFound when no record has id.
	GetRIExchangeRecord(ctx context.Context, id string) (*RIExchangeRecord, error)
	GetRIExchangeRecordByToken(ctx context.Context, token string) (*RIExchangeRecord, error)
	GetRIExchangeHistory(ctx context.Context, since time.Time, limit int) ([]RIExchangeRecord, error)
	// GetRIExchangeHistoryForAccounts is GetRIExchangeHistory restricted to
	// records whose account_id is in accountIDs, with the limit applied after
	// that restriction. An empty accountIDs matches no records.
	GetRIExchangeHistoryForAccounts(ctx context.Context, since time.Time, limit int, accountIDs []string) ([]RIExchangeRecord, error)
	// TransitionRIExchangeStatus atomically transitions an RI exchange record status.
	// actor is the UUID of the user performing the transition (nil for system-initiated paths).
	// A lost transition returns a wrapped ErrRIExchangeExpired when the record
	// has expired, or ErrRIExchangeNotInExpectedStatus when its status moved on.
	TransitionRIExchangeStatus(ctx context.Context, id string, fromStatus string, toStatus string, actor *string) (*RIExchangeRecord, error)
	CompleteRIExchange(ctx context.Context, id string, exchangeID string) error
	// CompleteRIExchangeWithPayment marks an RI exchange as completed and
	// updates payment_due to the amount AWS actually accepted. Use this instead
	// of CompleteRIExchange on the manual-approval path so the daily-spend
	// ledger (GetRIExchangeDailySpend) reflects the real accepted amount rather
	// than the stale pre-execution quote (H3 fix).
	CompleteRIExchangeWithPayment(ctx context.Context, id string, exchangeID string, acceptedPaymentDue string) error
	// StampRIExchangeApprovedBy sets the approved_by column on a completed
	// exchange row (issue #300). Called after CompleteRIExchangeWithPayment when
	// the approval came from a session-authed user rather than an email token.
	StampRIExchangeApprovedBy(ctx context.Context, id string, approverEmail string) error
	FailRIExchange(ctx context.Context, id string, errorMsg string) error
	GetRIExchangeDailySpend(ctx context.Context, date time.Time) (string, error)
	CancelAllPendingExchanges(ctx context.Context) (int64, error)
	// CancelPendingExchangesByOrigin cancels only pending records whose origin
	// matches:
	//   - common.ExchangeOriginStandalone: cancels WHERE ladder_run_id IS NULL
	//   - common.ExchangeOriginLadder:     cancels WHERE ladder_run_id IS NOT NULL
	// The origin is validated at the boundary and an unknown value fails loud.
	// This prevents the standalone ri_exchange_reshape task from wiping out
	// ladder-linked pending reshapes and vice versa (gap G10 / issue #1348).
	CancelPendingExchangesByOrigin(ctx context.Context, origin common.ExchangeOrigin) (int64, error)
	GetStaleProcessingExchanges(ctx context.Context, olderThan time.Duration) ([]RIExchangeRecord, error)

	// RI exchange direct-execute audit trail (issue #82). Exchanges executed
	// directly through the API, not through the approval flow above, are
	// recorded in their own table: SaveRIExchangeDirectExecution inserts the
	// row as "submitted" before the provider call, and
	// FinalizeRIExchangeDirectExecution records the outcome on it exactly
	// once (ErrRIExchangeDirectExecutionNotSubmitted when the row is absent or
	// already finalized). Neither touches the approval flow's spend ledger.
	SaveRIExchangeDirectExecution(ctx context.Context, record *RIExchangeDirectExecution) error
	FinalizeRIExchangeDirectExecution(ctx context.Context, id string, outcome RIExchangeDirectOutcome) error

	// Cloud accounts
	CreateCloudAccount(ctx context.Context, account *CloudAccount) error
	GetCloudAccount(ctx context.Context, id string) (*CloudAccount, error)
	// GetCloudAccountByExternalID looks up a cloud account by its
	// (provider, external_id) pair. Used by the scheduler's ambient
	// collector path to tag rec rows with the registered host-account's
	// UUID when the Lambda's STS identity matches a registered (but
	// possibly disabled) account — so the approve-modal Account column
	// shows the account name instead of `(ambient)`. Returns
	// (nil, nil) when no row matches (the caller treats this as the
	// genuine orphan case). The underlying
	// `UNIQUE(provider, external_id)` constraint on cloud_accounts
	// guarantees the lookup hits an index.
	GetCloudAccountByExternalID(ctx context.Context, provider, externalID string) (*CloudAccount, error)
	UpdateCloudAccount(ctx context.Context, account *CloudAccount) error
	DeleteCloudAccount(ctx context.Context, id string) error
	ListCloudAccounts(ctx context.Context, filter CloudAccountFilter) ([]CloudAccount, error)

	// Account credentials (encrypted blobs; never returned via API)
	SaveAccountCredential(ctx context.Context, accountID, credentialType, encryptedBlob string) error
	// GetAccountCredential returns ErrNotFound when the account has no
	// credential of credentialType.
	GetAccountCredential(ctx context.Context, accountID, credentialType string) (string, error)
	DeleteAccountCredentials(ctx context.Context, accountID string) error
	HasAccountCredentials(ctx context.Context, accountID string) (bool, error)

	// Account service overrides
	GetAccountServiceOverride(ctx context.Context, accountID, provider, service string) (*AccountServiceOverride, error)
	SaveAccountServiceOverride(ctx context.Context, override *AccountServiceOverride) error
	// SaveAccountServiceOverrideTx is SaveAccountServiceOverride inside the
	// caller's WithTx transaction.
	SaveAccountServiceOverrideTx(ctx context.Context, tx pgx.Tx, override *AccountServiceOverride) error
	DeleteAccountServiceOverride(ctx context.Context, accountID, provider, service string) error
	ListAccountServiceOverrides(ctx context.Context, accountID string) ([]AccountServiceOverride, error)

	// Plan ↔ account association
	SetPlanAccounts(ctx context.Context, planID string, accountIDs []string) error
	GetPlanAccounts(ctx context.Context, planID string) ([]CloudAccount, error)

	// Recommendations cache (ADR: store recommendations in Postgres so the
	// dashboard serves provider-switch clicks from SQL instead of live cloud
	// API calls). UpsertRecommendations is the only write path and takes a
	// list of (provider, account) pairs that successfully collected this
	// cycle. Stale-row eviction is scoped to that union so a partially-failed
	// provider preserves the failed accounts' previous-cycle rows. See
	// SuccessfulCollect for the per-row semantics. There is deliberately no
	// unscoped "replace everything" method (issue #159).
	UpsertRecommendations(ctx context.Context, collectedAt time.Time, recs []RecommendationRecord, successfulCollects []SuccessfulCollect) error
	ListStoredRecommendations(ctx context.Context, filter RecommendationFilter) ([]RecommendationRecord, error)
	// ListStoredRecommendationsChunk is ListStoredRecommendations for a
	// keyset walk: at most limit rows after the position after, in
	// byte-wise ID order then primary-key order (issue #981).
	ListStoredRecommendationsChunk(ctx context.Context, filter RecommendationFilter, after *RecommendationScanKey, limit int) (RecommendationChunk, error)
	GetRecommendationsFreshness(ctx context.Context) (*RecommendationsFreshness, error)
	SetRecommendationsCollectionError(ctx context.Context, errMsg string) error
	// SetRecommendationsCollectionPartial records (or, with nil, clears) the
	// most recent collection's incomplete-sweep summary.
	// UpsertRecommendations clears it, so a later complete collection drops
	// the marker.
	SetRecommendationsCollectionPartial(ctx context.Context, partial *RecommendationsCollectionPartial) error
	// MarkCollectionStarted atomically sets last_collection_started_at = now
	// only when no in-flight collection is running (last_collection_started_at IS NULL
	// OR older than 5 minutes), and stamps a freshly generated owner token
	// alongside it. Returns the token and true when this caller won the race
	// and should proceed with the async invoke, passing the token through so
	// only this caller can later clear the marker; ("", false, nil) when
	// another collection is already in flight and the caller should return
	// 409.
	MarkCollectionStarted(ctx context.Context) (token string, ok bool, err error)
	// ClearCollectionStarted clears last_collection_started_at, but only if
	// last_collection_owner_id still matches token: a compare-and-clear
	// guard (issue #261) so a caller that never won MarkCollectionStarted
	// (cron, cold-start) cannot wipe another caller's in-flight marker. A
	// mismatched token is a documented silent no-op (the marker belongs to
	// someone else); an empty token is a boundary error, since only a
	// caller that actually owns a marker should ever call Clear.
	ClearCollectionStarted(ctx context.Context, token string) error
	// ClaimCollectionInvocation is called by a token-carrying collect run
	// once it holds the task advisory lock (issue #84). It re-stamps the
	// marker with a fresh invocation-scoped token when the marker is still
	// owned by markerToken, or free/stale, so a lock-skipped duplicate
	// delivery of the same event (same markerToken) can no longer clear
	// it. Returns the new token and true when the caller now owns the
	// marker; ("", false, nil) when another run holds a fresh marker.
	ClaimCollectionInvocation(ctx context.Context, markerToken string) (token string, ok bool, err error)

	// RI utilization cache. Postgres-backed TTL cache for Cost Explorer
	// GetReservationUtilization; shared across Lambda containers so
	// dashboard loads don't each fan out to a paid CE API call. A
	// per-process in-memory cache effectively never hits because each
	// cold container starts empty. GetRIUtilizationCache returns nil
	// when the (region, lookback_days) key is absent — callers treat
	// that as a miss and re-fetch.
	GetRIUtilizationCache(ctx context.Context, region string, lookbackDays int) (*RIUtilizationCacheEntry, error)
	UpsertRIUtilizationCache(ctx context.Context, region string, lookbackDays int, payload []byte, fetchedAt time.Time) error

	// Account registrations (self-service enrollment via federation IaC)
	CreateAccountRegistration(ctx context.Context, reg *AccountRegistration) error
	GetAccountRegistration(ctx context.Context, id string) (*AccountRegistration, error)
	GetAccountRegistrationByToken(ctx context.Context, token string) (*AccountRegistration, error)
	ListAccountRegistrations(ctx context.Context, filter AccountRegistrationFilter) ([]AccountRegistration, error)
	UpdateAccountRegistration(ctx context.Context, reg *AccountRegistration) error
	// TransitionRegistrationStatus atomically updates a registration's workflow fields.
	// actor is the UUID of the reviewer (nil for system-initiated transitions).
	TransitionRegistrationStatus(ctx context.Context, reg *AccountRegistration, fromStatus string, actor *string) error
	DeleteAccountRegistration(ctx context.Context, id string) error

	// Purchase suppressions. Written inside a WithTx block during bulk
	// purchase submit so the execution insert + the suppression rows
	// commit atomically. Deleted on cancel/expire of the execution,
	// also inside a WithTx block paired with the status update.
	//
	// The plain variants (no Tx) open their own single-call transaction
	// — useful for tests and one-off admin operations. The Tx variants
	// reuse a caller-provided transaction so multi-write operations
	// can roll back atomically.
	CreateSuppression(ctx context.Context, sup *PurchaseSuppression) error
	CreateSuppressionTx(ctx context.Context, tx pgx.Tx, sup *PurchaseSuppression) error
	DeleteSuppressionsByExecution(ctx context.Context, executionID string) error
	DeleteSuppressionsByExecutionTx(ctx context.Context, tx pgx.Tx, executionID string) error
	ListActiveSuppressions(ctx context.Context) ([]PurchaseSuppression, error)

	// SavePurchaseExecutionTx is the tx-accepting variant of
	// SavePurchaseExecution. Used from executePurchase's WithTx block
	// so the execution insert + suppression writes commit atomically.
	SavePurchaseExecutionTx(ctx context.Context, tx pgx.Tx, execution *PurchaseExecution) error

	// SaveExecutionIfStatusTx and RecoverReapedExecutionTx are the
	// tx-accepting variants of SaveExecutionIfStatus and RecoverReapedExecution.
	SaveExecutionIfStatusTx(ctx context.Context, tx pgx.Tx, execution *PurchaseExecution, expected []string) error
	RecoverReapedExecutionTx(ctx context.Context, tx pgx.Tx, execution *PurchaseExecution) error

	// ListRecentSubmitsTx takes a per-creator transaction-scoped lock and
	// returns the creator's web-submitted executions scheduled at or after
	// since that are pending, in flight or bought. executePurchase's
	// duplicate guard reads it inside the WithTx block that inserts the new
	// execution, so concurrent identical submits serialize and a retry
	// that arrives after a direct execute left "pending" is still seen.
	ListRecentSubmitsTx(ctx context.Context, tx pgx.Tx, creatorID string, since time.Time) ([]PurchaseExecution, error)

	// WithTx opens a pgx transaction, runs fn, and commits on success or
	// rolls back on error. fn can call any *Tx method on the store to
	// participate in the transaction. Nested transactions are not
	// supported — fn must not call WithTx recursively.
	WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error

	// Ladder configuration (per-account, per-provider).
	// GetLadderConfigs returns all rows, newest first.
	// GetLadderConfig returns the single row for (cloudAccountID, provider),
	// or (nil, nil) when no row exists.
	// UpsertLadderConfig inserts or updates via the UNIQUE(cloud_account_id, provider)
	// constraint and returns the persisted row with all DB-stamped fields populated.
	GetLadderConfigs(ctx context.Context) ([]LadderConfigDB, error)
	GetLadderConfig(ctx context.Context, cloudAccountID, provider string) (*LadderConfigDB, error)
	UpsertLadderConfig(ctx context.Context, cfg *LadderConfigDB) (*LadderConfigDB, error)

	// Ladder run/tranche persistence (migration 000080/000081, PR-2).
	//
	// SaveLadderRun inserts a new ladder_runs row, returning the persisted row
	// with all DB-stamped fields (id, created_at, updated_at) populated.
	// If run.ID is empty, a new UUID is generated before the insert.
	//
	// GetLadderRun returns the row for the given id, or (nil, nil) when no
	// row exists (mirrors GetLadderConfig semantics).
	//
	// SaveLadderTranches inserts a batch of ladder_tranches rows inside a
	// single transaction. Each tranche must carry a non-empty ID; duplicate
	// IDs within the batch are rejected at the DB UNIQUE constraint level.
	//
	// LatestLadderRunStartedAt returns the maximum started_at for the given
	// config_id, or nil when no run has been recorded yet. Powers the per-cadence
	// self-gate in the scheduler (Q6).
	//
	// TransitionLadderRunStatus atomically updates the status of a ladder_runs
	// row from one of the fromStatuses to toStatus, returning the updated row.
	// Returns (nil, nil) when zero rows are affected (CAS race lost or wrong
	// current status), so callers can distinguish a race from a hard error.
	// Statuses are typed ladder.RunStatus so callers cannot pass an arbitrary
	// string that would never match a stored status. A transition to
	// RunStatusCancelled is refused (ErrLadderRunCancelViaTransition).
	//
	// CancelLadderRun is the only way to cancel a ladder run: a CAS UPDATE
	// from one of fromStatuses to cancelled that stamps cancelled_by with the
	// actor's email in the same statement. An empty actor is refused
	// (ErrLadderRunCancelActorRequired); (nil, nil) means the CAS lost.
	// PlanAndSaveLadderRun is the ladder planning persist: in ONE READ COMMITTED
	// transaction under the config's advisory lock it reads the in-flight
	// commitment (scheduled rows within LadderFireGrace plus fired rows), calls
	// build with it, then inserts the run, sweeps overdue tranches and inserts
	// the new ones (append-only, L5). Overlapping runs for one config serialize:
	// the second nets the first's committed rows. build must be pure compute
	// (no I/O); run.StartedAt must equal asOf. asOf MUST be captured before the
	// caller reads the existing commitment (E): tranches completed after asOf
	// are added to the in-flight figure, so one that completes between the E
	// read and the locked read is netted, never missed (a double count is a
	// safe under-buy). This holds only if a tranche is marked completed after
	// its commitment is visible through E's source (executor PR5a). The lock is
	// also taken first by ClaimDueLadderTranches.
	PlanAndSaveLadderRun(ctx context.Context, configID string, asOf time.Time, build LadderPlanBuilder) (*LadderRunDB, error)
	SaveLadderRun(ctx context.Context, run *LadderRunDB) (*LadderRunDB, error)
	GetLadderRun(ctx context.Context, id string) (*LadderRunDB, error)
	SaveLadderTranches(ctx context.Context, tranches []LadderTrancheDB) error
	LatestLadderRunStartedAt(ctx context.Context, configID string) (*time.Time, error)
	TransitionLadderRunStatus(ctx context.Context, id string, fromStatuses []ladder.RunStatus, toStatus ladder.RunStatus) (*LadderRunDB, error)
	CancelLadderRun(ctx context.Context, id string, fromStatuses []ladder.RunStatus, cancelledBy string) (*LadderRunDB, error)

	// Notification mutes (issue #297 / migration 000091).
	// UpsertNotificationMute inserts or updates a mute row for (email, scope).
	// Idempotent for row existence: calling it again for an already-muted
	// address refreshes muted_at and replaces unmute_token if the token changes.
	UpsertNotificationMute(ctx context.Context, recipientEmail, scope, unmuteToken string) error
	// IsNotificationMuted returns true when (email, scope) has a row in
	// muted_recipients. The email comparison is case-insensitive.
	IsNotificationMuted(ctx context.Context, recipientEmail, scope string) (bool, error)
}
