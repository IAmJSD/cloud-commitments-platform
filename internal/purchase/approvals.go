package purchase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/jackc/pgx/v5"
)

// ErrFourEyesDenied wraps every refusal by the 4-eyes approval policy, as
// opposed to a failure to evaluate it (config or store errors).
var ErrFourEyesDenied = errors.New("approval declined")

// ErrInvalidApprovalToken and ErrApprovalTokenExpired let the HTTP layer map a
// bad email-link token to a 4xx instead of a 500.
var (
	ErrInvalidApprovalToken = errors.New("invalid approval token")
	ErrApprovalTokenExpired = errors.New("approval token has expired")
)

// runNowSkipReason is stamped onto PreApprovalSkipReason when an operator's
// "Run now" stands in for the approval and the scheduled wait.
const runNowSkipReason = "run-now"

// directExecuteSkipReason is stamped onto PreApprovalSkipReason when a session
// holding the direct-execute permission skips the approval round-trip.
const directExecuteSkipReason = "direct-execute permission"

// ApproveExecution is the token-authenticated approve entry point used by
// the legacy email-link flow and the SQS approve worker. After validating
// the approval token it hands off to ApproveAndExecute, which performs the
// atomic status transition, runs the AWS purchase synchronously, and mints
// the fresh revocation token returned here.
//
// actor carries the email of the operator who triggered the approval
// (session-authed click on the HTTP path; verified actor_email on the SQS
// path) so it can be stamped onto ApprovedBy. Empty actor is recorded as
// NULL — the column is nullable TEXT and we don't want to claim "approved
// by nobody".
//
// check is the caller's ClaimedRowCheck, run against the claimed row before
// any provider call (issue #415); the SQS approve worker passes nil.
//
// Returns the raw revocation token minted by ApproveAndExecute on success
// (see that function's doc comment for why this must be a return value
// rather than something the caller re-fetches from the DB). Empty on
// failure, and best-effort empty if the mint itself failed after an
// otherwise-successful approve. A partially_completed run returns its token
// together with its error (issue #532), so a non-empty token with an error
// means part of the purchase committed and is revocable.
func (m *Manager) ApproveExecution(ctx context.Context, executionID, token, actor string, check ClaimedRowCheck) (string, error) {
	t0 := time.Now()
	logging.Infof("purchase[%s]: ApproveExecution entry (auth=token actor=%q)", executionID, maskActor(actor))

	execution, err := m.config.GetExecutionByID(ctx, executionID)
	if errors.Is(err, config.ErrNotFound) {
		return "", fmt.Errorf("execution not found: %s", executionID)
	}
	if err != nil {
		return "", fmt.Errorf("failed to get execution: %w", err)
	}

	// Validate token and TTL (Finding #4 + issue #397).
	if tokErr := ValidateApprovalToken(execution, token); tokErr != nil {
		return "", tokErr
	}

	// Preflight guard (issue #609): reject non-AWS orphan executions before
	// the cloud SDK is reached. See OrphanExecutionError for the full rationale.
	if checkErr := OrphanExecutionError(execution); checkErr != nil {
		logging.Errorf("purchase[%s]: ApproveExecution preflight rejected after %s: %v",
			executionID, time.Since(t0), checkErr)
		return "", checkErr
	}

	// Token/SQS path: no authenticated session UUID is available, so the
	// transition is recorded as system-initiated (transitioned_by = NULL).
	_, revocationToken, err := m.ApproveAndExecute(ctx, executionID, actor, nil, check)
	if err != nil {
		logging.Errorf("purchase[%s]: ApproveExecution (token path) failed after %s: %v",
			executionID, time.Since(t0), err)
	} else {
		logging.Infof("purchase[%s]: ApproveExecution (token path) completed in %s",
			executionID, time.Since(t0))
	}
	return revocationToken, err
}

// maskActor masks an actor email/username for safe log emission.
// Full email addresses are PII; log only the domain part.
// Empty actor is logged as "<anon>" so the distinction between
// "no actor provided" and "actor was empty string" is visible.
func maskActor(actor string) string {
	if actor == "" {
		return "<anon>"
	}
	if at := len(actor) - 1; at >= 0 {
		for i, c := range actor {
			if c == '@' {
				return "***" + actor[i:]
			}
		}
	}
	// Not an email address -- log only the last 4 chars (e.g. a username).
	if len(actor) > 4 {
		return "..." + actor[len(actor)-4:]
	}
	return "****"
}

// ValidateApprovalToken checks that the execution carries a non-empty token,
// that the supplied token matches the stored one using constant-time comparison
// (Finding #4 -- prevents timing attacks), and that the token has not expired
// (issue #397). Legacy rows with a nil ApprovalTokenExpiresAt pass the TTL
// check for backward compatibility. Extracted from ApproveExecution to keep
// that function under the gocyclo threshold. Exported so the HTTP layer's
// delayed-approval branch (which never calls ApproveExecution) validates the
// token the same way (issue #586).
//
// execution.ApprovalToken is the SHA-256 hex digest stored at rest
// (issue #103), never the raw secret; config.ApprovalTokenMatches hashes the
// supplied token and compares digests in constant time.
func ValidateApprovalToken(execution *config.PurchaseExecution, token string) error {
	if execution.ApprovalToken == "" || token == "" {
		return ErrInvalidApprovalToken
	}
	if !config.ApprovalTokenMatches(execution.ApprovalToken, token) {
		return ErrInvalidApprovalToken
	}
	if execution.ApprovalTokenExpiresAt != nil && time.Now().After(*execution.ApprovalTokenExpiresAt) {
		return ErrApprovalTokenExpired
	}
	return nil
}

// OrphanExecutionError returns a descriptive error when the execution has no
// resolvable CloudAccountID and the provider is explicitly non-AWS (issue #609).
// AWS executions with a nil CloudAccountID are passed through because the
// ambient-host-account fallback from PR #607/#604 handles them. Empty
// provider is treated as AWS (legacy rows pre-dating multi-cloud support).
//
// An execution is NOT considered orphan if any individual recommendation
// carries its own non-nil CloudAccountID — multi-rec fan-out executions can
// have a nil execution-level field while individual recs still name the
// target account.
//
// Exported so the HTTP layer (internal/api) can call the same guard without
// duplicating the predicate. Extracted from ApproveExecution to keep that
// function below the gocyclo threshold — same pattern as loadCancelableExecution.
func OrphanExecutionError(execution *config.PurchaseExecution) error {
	if execution.CloudAccountID != nil {
		return nil
	}
	if len(execution.Recommendations) == 0 {
		return nil
	}
	provider := execution.Recommendations[0].Provider
	if provider == "" || provider == "aws" {
		return nil
	}
	// Any rec-level CloudAccountID means at least one recommendation has a
	// concrete target account — the execution is not fully orphaned.
	for i := range execution.Recommendations {
		if execution.Recommendations[i].CloudAccountID != nil {
			return nil
		}
	}
	return fmt.Errorf("execution %s references an account that no longer exists (provider: %s): cancel this purchase — it cannot execute",
		execution.ExecutionID, provider)
}

// enforceFourEyesPolicy is the UNIVERSAL 4-eyes approval gate (issue #1005).
// It is the single choke point ApproveAndExecute runs before mutating any
// execution state, so every approve/execute entry point inherits the policy
// regardless of which caller reaches ApproveAndExecute:
//
//   - approvePurchaseViaSession (session-authed dashboard approve)
//   - directExecutePurchase (execute_mode="direct" -- issue #289)
//   - ApproveExecution (email-token deep-link approve AND the SQS async
//     approve worker, both of which funnel into ApproveAndExecute)
//
// Adversarial review of PR #1500 found that requireDifferentApprover in
// internal/api was wired into only 2 of these 4 entry points, leaving
// execute_mode="direct" (HIGH: the requester always equals the creator on
// this path, so this is unconditional self-authorization) and the SQS
// approve worker (MEDIUM: token + actor_email verification never compared
// the actor against the creator) able to bypass dual control entirely. This
// method closes both gaps by running at the one place all four paths share.
//
// Identity comparison has two tiers, checked in order of precision:
//
//  1. actorUserID (== transitionedBy, the actor's own UUID): populated by the
//     two session-based callers (approvePurchaseViaSession, directExecutePurchase)
//     via validUUIDPtrOrNil(&session.UserID). When present it is compared
//     DIRECTLY against execution.CreatedByUserID -- both are UUIDs from the
//     same `users` table, so this is authoritative identity, not a proxy.
//     This tier is what closes a gap an independent adversarial review found
//     in an earlier version of this fix: a per-user API key session
//     (Session.UserAPIKeyID != "") carries a real user UUID but an EMPTY
//     Session.Email, unlike a normal bearer-token session. An email-only
//     comparison would fall back to a non-email placeholder for that empty
//     Email and could never match the creator's real email, silently
//     allowing self-direct-execute. Comparing UUIDs first sidesteps the
//     email domain entirely for these two callers and cannot be fooled by an
//     empty or substituted email string.
//  2. actorEmail: used only when actorUserID is nil -- the token
//     (approveViaToken) and SQS (handleApproveMessage) callers into
//     ApproveExecution, which always pass transitionedBy=nil (see
//     ApproveExecution's own doc comment), so the RBAC/contact-email-
//     verified actor identity from authorizeApprovalAction /
//     verifyAsyncApprovalActor is the only signal available there.
//     execution.CreatedByUserID is a UUID; internal/purchase cannot resolve
//     it through internal/auth directly (internal/auth already imports
//     internal/config, so the reverse import would cycle), so
//     GetUserEmailByID resolves the creator's email via a minimal,
//     auth-type-free query on the shared config store instead.
//
// Fails CLOSED whenever mode is on and any of the following holds: the
// execution has no recorded creator (legacy row); neither actorUserID nor a
// non-empty actorEmail identify the acting party; or the resolved identity
// (UUID or email) matches the creator's.
func (m *Manager) enforceFourEyesPolicy(ctx context.Context, executionID, actorEmail string, actorUserID *string) error {
	cfg, err := m.config.GetGlobalConfig(ctx)
	if err != nil {
		return fmt.Errorf("4-eyes policy check: failed to load global config: %w", err)
	}
	if cfg == nil || !cfg.RequireDifferentApprover {
		return nil
	}

	execution, err := m.loadExecutionForFourEyes(ctx, executionID)
	if err != nil || execution == nil {
		return err
	}
	return m.checkDifferentApprover(ctx, executionID, execution, actorEmail, actorUserID)
}

// loadExecutionForFourEyes fetches the execution enforceFourEyesPolicy needs
// to compare identities against. Returns (nil, nil) when the executionID
// does not resolve to a row -- there is nothing to gate, and the caller's
// subsequent TransitionExecutionStatus surfaces the standard not-found/
// cannot-transition error for a bogus or already-terminal executionID.
// Extracted from enforceFourEyesPolicy to keep it under the gocyclo
// threshold.
func (m *Manager) loadExecutionForFourEyes(ctx context.Context, executionID string) (*config.PurchaseExecution, error) {
	execution, err := m.config.GetExecutionByID(ctx, executionID)
	if errors.Is(err, config.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("4-eyes policy check: failed to load execution: %w", err)
	}
	return execution, nil
}

// checkSystemCreatedApprover gates a scheduler-minted execution (issue #233):
// there is no human requester to differ from, so any identified approver
// passes and an anonymous actor is denied (fail-closed).
func (m *Manager) checkSystemCreatedApprover(executionID, actorEmail string, actorUserID *string) error {
	if actorUserID == nil && strings.TrimSpace(actorEmail) == "" {
		logging.Warnf("purchase[%s]: 4-eyes mode on; system-created row, no actor identity available, denying (fail-closed)", executionID)
		return fmt.Errorf("%w: 4-eyes approval mode is enabled but no approver identity could be determined; sign in or supply a verified actor before approving", ErrFourEyesDenied)
	}
	logging.Infof("purchase[%s]: 4-eyes mode on; system-created row has no human requester, approver accepted", executionID)
	return nil
}

// checkDifferentApprover runs the actual identity comparison once mode is
// confirmed on and the execution is loaded. Extracted from
// enforceFourEyesPolicy to keep it under the gocyclo threshold.
func (m *Manager) checkDifferentApprover(ctx context.Context, executionID string, execution *config.PurchaseExecution, actorEmail string, actorUserID *string) error {
	if execution.CreatedByUserID == nil && execution.CreatedBySystem {
		return m.checkSystemCreatedApprover(executionID, actorEmail, actorUserID)
	}
	if execution.CreatedByUserID == nil {
		logging.Warnf("purchase[%s]: 4-eyes mode on; NULL creator (legacy row), denying", executionID)
		return fmt.Errorf("%w: this execution predates the dual-control feature and has no recorded creator; an admin must disable 4-eyes mode to approve", ErrFourEyesDenied)
	}

	// Tier 1: authoritative UUID comparison when the caller identified the
	// actor's own user row (session-based callers). See enforceFourEyesPolicy's
	// doc comment for why this must run before any email-based fallback.
	if actorUserID != nil {
		if *actorUserID == *execution.CreatedByUserID {
			logging.Warnf("purchase[%s]: 4-eyes mode on; creator %s attempted self-approval (actor UUID match), denied",
				executionID, *execution.CreatedByUserID)
			return fmt.Errorf("%w: 4-eyes mode requires a different approver than the requester", ErrFourEyesDenied)
		}
		return nil
	}

	// Tier 2: no actor UUID available (token/SQS callers) -- fall back to
	// resolving and comparing emails.
	actorEmail = strings.TrimSpace(actorEmail)
	if actorEmail == "" {
		logging.Warnf("purchase[%s]: 4-eyes mode on; no actor identity available, denying (fail-closed)", executionID)
		return fmt.Errorf("%w: 4-eyes approval mode is enabled but no approver identity could be determined; sign in or supply a verified actor before approving", ErrFourEyesDenied)
	}

	creatorEmail, err := m.config.GetUserEmailByID(ctx, *execution.CreatedByUserID)
	if err != nil {
		return fmt.Errorf("4-eyes policy check: failed to resolve creator identity: %w", err)
	}
	creatorEmail = strings.TrimSpace(creatorEmail)
	if creatorEmail == "" {
		logging.Warnf("purchase[%s]: 4-eyes mode on; creator account %s not found, denying (fail-closed)",
			executionID, *execution.CreatedByUserID)
		return fmt.Errorf("%w: 4-eyes approval mode is enabled but the creator's account could not be resolved; an admin must investigate before approving", ErrFourEyesDenied)
	}

	if strings.EqualFold(creatorEmail, actorEmail) {
		logging.Warnf("purchase[%s]: 4-eyes mode on; creator %s attempted self-approval via actor %q, denied",
			executionID, *execution.CreatedByUserID, maskActor(actorEmail))
		return fmt.Errorf("%w: 4-eyes mode requires a different approver than the requester", ErrFourEyesDenied)
	}
	return nil
}

// ApproveAndExecute atomically flips a pending/notified execution to
// "approved" (stamping ApprovedBy) and then runs the purchase
// synchronously. Callers MUST have already authorized the actor — this
// method does no RBAC or token validation; it is shared between:
//
//   - ApproveExecution (token/SQS path: token validated by the caller)
//   - approvePurchaseViaSession (session path: RBAC validated by the caller)
//   - directExecutePurchase (execute_mode="direct": RBAC validated by the caller)
//
// Concurrency: TransitionExecutionStatus uses an atomic UPDATE WHERE status
// IN ('pending','notified'). Two callers racing to approve the same row
// will see exactly one win — the loser receives a clear "cannot
// transition" error. Cross-execution concurrency is unaffected: each
// approval drives its own executeAndFinalize, which already fans out
// per-account in parallel via executeMultiAccount.
//
// The returned row carries the approval's final status, also on an execution
// error; it is nil when the run never started. check runs against the claimed
// row before any provider call; see ClaimedRowCheck.
func (m *Manager) ApproveAndExecute(ctx context.Context, executionID, actor string, transitionedBy *string, check ClaimedRowCheck) (*config.PurchaseExecution, string, error) {
	return m.transitionApproveAndExecute(ctx, executionID, actor, transitionedBy, []string{"pending", "notified"}, "", check)
}

// DirectExecute is ApproveAndExecute for a session with the direct-execute
// permission: the same 4-eyes-gated, CAS-guarded funnel, but it stamps the
// executed_* audit fields once the CAS has won, so a refused or lost direct
// execute leaves no executed_at on a row that never ran.
func (m *Manager) DirectExecute(ctx context.Context, executionID, actor string, transitionedBy *string, check ClaimedRowCheck) (*config.PurchaseExecution, string, error) {
	return m.transitionApproveAndExecute(ctx, executionID, actor, transitionedBy, []string{"pending", "notified"}, directExecuteSkipReason, check)
}

// RunPlannedPurchaseNow lets an operator force a scheduled purchase (pending
// or paused) to execute immediately instead of waiting for its scheduled
// date, going through the exact same 4-eyes gate and synchronous
// executeAndFinalize funnel ApproveAndExecute uses (issue #218). The
// previous runPlannedPurchase implementation only flipped the row's status
// to "running" and returned success: no executor consumes "running" rows
// (the scheduler picks up pending/notified, the reaper only cleans up a
// stuck "running" row after 10 minutes), so nothing ever bought the
// purchase and the row later reads "failed" out from under the operator who
// was told it ran.
//
// Sharing transitionApproveAndExecute's CAS with ApproveAndExecute and
// claimAndExecute is also what makes this idempotent: a second "Run now"
// click, or the scheduler racing the same pending row, loses the atomic
// UPDATE ... WHERE status IN (...) and gets a clean "cannot transition"
// error rather than executing the purchase a second time.
//
// The returned row carries the run-now audit stamp and its final status, also
// on an execution error; it is nil when the run never started. check runs
// against the claimed row before any provider call; see ClaimedRowCheck.
func (m *Manager) RunPlannedPurchaseNow(ctx context.Context, executionID, actor string, transitionedBy *string, check ClaimedRowCheck) (*config.PurchaseExecution, string, error) {
	return m.transitionApproveAndExecute(ctx, executionID, actor, transitionedBy, []string{"pending", "notified", "paused"}, runNowSkipReason, check)
}

// transitionApproveAndExecute is the shared body behind ApproveAndExecute
// and RunPlannedPurchaseNow: enforce the 4-eyes gate, atomically CAS the row
// from one of fromStatuses straight to "running" (the claim), stamp ApprovedBy, then run the
// purchase synchronously. Parameterizing on fromStatuses lets both entry
// points share one implementation of the gate and the CAS instead of the
// gate being reimplemented (or skipped) at a second call site.
//
// Returns the raw revocation token minted on a successful execute, or ""
// if the (best-effort) mint failed. A partially_completed run also gets one,
// returned alongside its error (see IsCommittedPartialRun, issue #532).
// Every caller emails a "purchase executed, revoke here" link, and since
// only the token's hash is stored (issue #103) a DB re-read can never yield
// a raw, emailable value, so the rotation lives here, in the one funnel all
// approve paths share.
//
// A non-empty skipReason stamps the executed_* audit fields once the CAS wins.
// The returned row is nil when the CAS was never won.
//
// check runs against the row the CAS returned, which is the row
// executeAndFinalize buys, before any audit stamp or provider call (issue
// #415). A refusal fails that row before submission (status "failed",
// no_submission set, a plan row's suppressions released) and returns it with
// an error satisfying errors.Is(err, ErrClaimedRowRefused).
func (m *Manager) transitionApproveAndExecute(ctx context.Context, executionID, actor string, transitionedBy *string, fromStatuses []string, skipReason string, check ClaimedRowCheck) (*config.PurchaseExecution, string, error) {
	t0 := time.Now()
	logging.Infof("purchase[%s]: transitionApproveAndExecute starting (actor=%q, from=%v)", executionID, maskActor(actor), fromStatuses)

	// Universal 4-eyes gate (issue #1005 / PR #1500 adversarial review): runs
	// before any state mutation so every caller -- session approve, direct
	// execute, run-now, token approve, SQS approve -- is covered by one
	// policy check. transitionedBy doubles as the actor's own UUID for this
	// check when the caller has one (session-based callers); see
	// enforceFourEyesPolicy's doc comment for the full rationale.
	if err := m.enforceFourEyesPolicy(ctx, executionID, actor, transitionedBy); err != nil {
		logging.Warnf("purchase[%s]: transitionApproveAndExecute denied by 4-eyes policy: %v", executionID, err)
		return nil, "", err
	}

	// transitionedBy carries the session user's UUID for human-initiated
	// approvals (stamped onto transitioned_by); it is nil for token/SQS/system
	// flows so transitioned_by = NULL on those hops. The human-readable actor
	// email is recorded separately onto approved_by (below).
	// One CAS straight to "running": winning it is the claim, so no other
	// executor (SQS, cron, re-drive, scheduler) can run this row, and there is
	// no window where the row sits "approved" with a stale copy in flight
	// (issue #829). A lost CAS surfaces ErrExecutionNotInExpectedStatus.
	updated, err := m.claimAndCheck(ctx, executionID, fromStatuses, transitionedBy, check, t0)
	if err != nil {
		return updated, "", err
	}
	logging.Infof("purchase[%s]: approved and claimed running in %s", executionID, time.Since(t0))

	if skipReason != "" {
		now := time.Now()
		reason := skipReason
		updated.ExecutedAt = &now
		updated.ExecutedByUserID = transitionedBy
		updated.PreApprovalSkipReason = &reason
	}
	if actor != "" {
		a := actor
		updated.ApprovedBy = &a
	}
	if actor != "" || skipReason != "" {
		if saveErr := m.config.SaveExecutionIfStatus(ctx, updated, executorOwnedStatuses); saveErr != nil {
			// Attribution is best-effort once the atomic flip has landed --
			// dropping these audit fields must not stop the purchase from firing.
			// Log loudly so the audit gap is visible.
			logging.Errorf("AUDIT GAP: failed to stamp approval audit fields on %s: %v", executionID, saveErr)
		}
	}

	logging.Infof("purchase[%s]: executing purchase synchronously", executionID)
	revocationToken, execErr := m.runClaimedAndMint(ctx, updated, t0)
	return updated, revocationToken, execErr
}

// IsCommittedPartialRun reports whether an execute call that returned row and
// err ended partially_completed with that status saved: some commitments were
// bought, so the run is revocable and its executed email must go out like a
// completed run's (issue #532). An audit loss is excluded: the stored row is
// still "running", whatever the returned row says.
func IsCommittedPartialRun(row *config.PurchaseExecution, err error) bool {
	return err != nil && row != nil && row.Status == config.StatusPartiallyCompleted && !errors.Is(err, config.ErrAuditLoss)
}

// runClaimedAndMint runs the claimed row and, when the run committed anything
// (completed, or a saved partially_completed), mints its revocation token. It
// returns the raw token ("" when nothing committed or the best-effort mint
// failed) and the run's error, which a partial run keeps.
func (m *Manager) runClaimedAndMint(ctx context.Context, updated *config.PurchaseExecution, t0 time.Time) (string, error) {
	executionID := updated.ExecutionID
	execErr := m.executeAndFinalize(ctx, updated)
	if execErr != nil {
		logging.Errorf("purchase[%s]: transitionApproveAndExecute failed after %s: %v", executionID, time.Since(t0), execErr)
		if !IsCommittedPartialRun(updated, execErr) {
			return "", execErr
		}
	} else {
		logging.Infof("purchase[%s]: transitionApproveAndExecute completed in %s", executionID, time.Since(t0))
	}

	// Mint a fresh revocation token now that the purchase has committed, in
	// full or in part (a partially_completed row is revocable, issue #532).
	// The pre-approve approval token must not double as the revocation token:
	// on the token path it was just consumed to authorize this very approve
	// action, and on every path its raw value is no longer recoverable once
	// hashed at rest (issue #103). See rotateApprovalToken's doc comment.
	revocationToken, mintErr := m.rotateApprovalToken(ctx, updated, config.RevocationWindow)
	if mintErr != nil {
		logging.Warnf("purchase[%s]: transitionApproveAndExecute: revocation token mint failed (best-effort): %v", executionID, mintErr)
	}
	return revocationToken, execErr
}

// CancelExecution cancels a pending execution. actor carries the email of
// the session-authenticated user who clicked cancel; verified by the
// caller (HTTP path: authorizeApprovalAction; SQS path:
// verifyAsyncApprovalActor) before reaching here. Same empty-actor
// rationale as ApproveExecution.
//
// Concurrency: CancelExecutionAtomic uses a conditional UPDATE WHERE
// status IN ('pending','notified') so a concurrent approve that wins
// the race causes zero rows to be affected and the caller receives a
// clean error with the current status rather than silently overwriting
// the approved row. This is the token/email-link cancel analog of
// the atomic guard TransitionExecutionStatus provides for ApproveAndExecute.
func (m *Manager) CancelExecution(ctx context.Context, executionID, token, actor string) error {
	logging.Infof("Canceling execution: %s", executionID)

	if _, err := m.loadCancelableExecution(ctx, executionID, token); err != nil {
		return err
	}

	// Build the nullable canceled_by pointer — see ApproveExecution for
	// the nil-vs-empty-string rationale.
	var cancelledBy *string
	if actor != "" {
		a := actor
		cancelledBy = &a
	}

	// Atomic conditional UPDATE + suppression cleanup in one transaction.
	// CancelExecutionAtomic flips status only when status IN
	// ('pending','notified') so a concurrent approve that has already
	// transitioned the row causes zero rows affected and we surface a 409.
	var canceled bool
	var currentStatus string
	if err := m.config.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		canceled, currentStatus, err = m.config.CancelExecutionAtomic(ctx, tx, executionID, cancelledBy)
		if err != nil {
			return err
		}
		if !canceled {
			// Row already transitioned (concurrent approve/cancel won the
			// race). Return early without touching suppressions — the other
			// operation owns the execution state now.
			return nil
		}
		return m.config.DeleteSuppressionsByExecutionTx(ctx, tx, executionID)
	}); err != nil {
		return fmt.Errorf("failed to cancel execution: %w", err)
	}

	if !canceled {
		return fmt.Errorf("%w: execution %s cannot be canceled: concurrent operation already transitioned it to %q", config.ErrExecutionNotInExpectedStatus, executionID, currentStatus)
	}

	logging.Infof("Execution %s canceled", executionID)
	// Token clear (best-effort): a canceled execution has no valid follow-up
	// action, so the approval token is cleared to prevent replay. Unlike the
	// approve path (which mints a fresh revocation token), cancel produces no
	// usable successor action, so an empty token is correct here.
	if rotateErr := m.clearApprovalToken(ctx, executionID); rotateErr != nil {
		logging.Warnf("purchase[%s]: CancelExecution: token clear failed (best-effort): %v", executionID, rotateErr)
	}
	return nil
}

// rotateApprovalToken generates a fresh cryptographically-secure token,
// mutates exec in place to hold its SHA-256 hash and a new ttl-based expiry
// (issue #103: only the token hash is stored, so only the digest is ever
// written to the DB), persists exec with a single SavePurchaseExecution
// call, and returns the RAW token for the caller to embed in an email.
//
// exec MUST already be the caller's current, freshly-loaded copy of the row
// -- this function does no fetch of its own. That is deliberate: an internal
// fetch-by-ID would allocate a SEPARATE struct from whatever the caller is
// already holding, and mutating that separate copy while the caller
// independently re-saves ITS OWN (still-stale) copy afterwards would silently
// overwrite this function's hash with the pre-rotation one -- exactly the
// class of two-writers-one-row bug this signature is designed to make
// impossible by construction.
//
// Its caller is transitionApproveAndExecute (ttl=RevocationWindow), after
// every successful execute, passing `updated` (mutated in place by the
// preceding TransitionExecutionStatus / executeAndFinalize calls), so the
// "purchase executed" email carries a valid revoke-capable token rather than
// the consumed or unrecoverable approval token. The pre-approval
// notification path rotates via RotatePendingApprovalToken instead, because
// it persists only after the email is sent and must not upsert a stale row.
//
// Best-effort: if the write fails the caller's own state change (the
// approve) has already landed, so this returns ("", err) and the caller only logs -- the
// email-link convenience is degraded for this row, not the underlying
// purchase state.
func (m *Manager) rotateApprovalToken(ctx context.Context, exec *config.PurchaseExecution, ttl time.Duration) (string, error) {
	if exec == nil {
		return "", fmt.Errorf("rotateApprovalToken: execution is nil")
	}
	tok, err := common.GenerateApprovalToken()
	if err != nil {
		return "", fmt.Errorf("rotateApprovalToken: failed to generate token: %w", err)
	}
	expiry := time.Now().Add(ttl)
	exec.ApprovalToken = config.HashApprovalToken(tok)
	exec.ApprovalTokenExpiresAt = &expiry
	if saveErr := m.config.SavePurchaseExecution(ctx, exec); saveErr != nil {
		return "", saveErr
	}
	return tok, nil
}

// clearApprovalToken clears the ApprovalToken on the execution after a
// successful cancel. A canceled execution has no valid follow-up action, so
// clearing the token prevents replay for any purpose. Unlike the approve path
// (which mints a fresh revocation token for the revoke-window email link),
// cancel produces no usable successor action.
//
// Best-effort: if the follow-up save fails the cancel has already landed, so
// we only warn rather than surfacing an error to the caller.
func (m *Manager) clearApprovalToken(ctx context.Context, executionID string) error {
	exec, err := m.config.GetExecutionByID(ctx, executionID)
	if err != nil {
		return fmt.Errorf("clearApprovalToken: failed to fetch execution: %w", err)
	}
	if exec == nil {
		return fmt.Errorf("clearApprovalToken: execution not found: %s", executionID)
	}
	exec.ApprovalToken = ""
	exec.ApprovalTokenExpiresAt = nil
	return m.config.SavePurchaseExecution(ctx, exec)
}

// loadCancelableExecution fetches an execution, validates the approval
// token, and checks the status is cancelable. Extracted from
// CancelExecution to keep both functions below the gocyclo threshold.
func (m *Manager) loadCancelableExecution(ctx context.Context, executionID, token string) (*config.PurchaseExecution, error) {
	execution, err := m.config.GetExecutionByID(ctx, executionID)
	if errors.Is(err, config.ErrNotFound) {
		return nil, fmt.Errorf("execution not found: %s", executionID)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get execution: %w", err)
	}
	// execution.ApprovalToken is the SHA-256 hex digest stored at rest
	// (issue #103); ValidateApprovalToken hashes the supplied token and
	// compares digests in constant time, and enforces the TTL (issue #397;
	// legacy rows without ApprovalTokenExpiresAt pass).
	if err := ValidateApprovalToken(execution, token); err != nil {
		return nil, err
	}

	// Only pre-approval rows (pending/notified) are cancelable -- shares the
	// single PurchaseExecution.IsCancelable predicate with the session path in
	// cancelPurchaseViaSession so the policy can never drift between the two
	// flows (issue #645). A scheduled row is revoked, not canceled: the atomic
	// cancel UPDATE matches only pending/notified, so letting it through would
	// report a "concurrent operation" that never happened (issue #73).
	// approved/running rows are mid-execution (the AWS commitment is being or
	// has been created), so canceling them would desync the DB from the cloud.
	if execution.Status == "scheduled" {
		return nil, fmt.Errorf("%w: execution cannot be canceled, current status: scheduled; use the revoke endpoint before it fires", config.ErrExecutionNotInExpectedStatus)
	}
	if !execution.IsCancelable() {
		return nil, fmt.Errorf("%w: execution cannot be canceled, current status: %s", config.ErrExecutionNotInExpectedStatus, execution.Status)
	}
	return execution, nil
}
