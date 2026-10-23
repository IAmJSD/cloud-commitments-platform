// Package purchase handles the purchase workflow including approvals and execution.
package purchase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/credentials"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/ctxutil"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/email"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/oidc"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/jackc/pgx/v5"
)

// STSClient interface for AWS STS operations.
type STSClient interface {
	GetCallerIdentity(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// ManagerConfig holds configuration for the purchase manager.
type ManagerConfig struct {
	AmbientAWSCreds        aws.CredentialsProvider
	EmailSender            email.SenderInterface
	STSClient              STSClient
	AssumeRoleSTS          credentials.STSClient
	CredentialStore        credentials.CredentialStore
	ProviderFactory        provider.FactoryInterface
	ConfigStore            config.StoreInterface
	OIDCSigner             oidc.Signer
	OIDCIssuerURL          string
	DefaultPaymentOption   string
	DefaultRampSchedule    string
	DashboardURL           string
	NotificationDaysBefore int
	DefaultCoverage        float64
	DefaultTerm            int
}

// Manager handles purchase workflow.
type Manager struct {
	config          config.StoreInterface
	email           email.SenderInterface
	stsClient       STSClient
	assumeRoleSTS   credentials.STSClient
	ambientAWSCreds aws.CredentialsProvider
	credStore       credentials.CredentialStore
	providerFactory provider.FactoryInterface
	oidcSigner      oidc.Signer
	defaults        PurchaseDefaults
	dashboardURL    string
	oidcIssuerURL   string
	notifyDays      int
	resolver        stepResolver
}

// PurchaseDefaults holds default purchase settings.
type PurchaseDefaults struct { //nolint:revive // exported: doc comment style intentional
	Payment      string
	RampSchedule string
	Term         int
	Coverage     float64
}

// ProcessResult holds the result of processing scheduled purchases.
type ProcessResult struct {
	Errors    []string `json:"errors,omitempty"`
	Processed int      `json:"processed"`
	Executed  int      `json:"executed"`
	Failed    int      `json:"failed"`
	Recovered int      `json:"recovered,omitempty"`
}

// staleApprovedThreshold is how long an execution may sit in the "approved"
// status before the recovery sweep in ProcessScheduledPurchases treats it as
// stranded (issue #632). It must be comfortably larger than the longest
// possible synchronous purchase run so a legitimately in-flight execution is
// never failed out from under itself. The purchase Lambda timeout is 60s;
// 15min (matching the RI-exchange stale-sweep threshold in pkg/exchange)
// leaves a wide safety margin.
//
// Note: the stuck-purchase reaper (reaper.go) fails "running" rows after
// DefaultReapAfter (10m) but leaves "approved" rows alone until
// minApprovedReapAfter, which is derived from this constant plus one process
// sweep interval, so this recovery always gets to re-drive a stranded
// approval before the reaper fails it (issue #894).
const staleApprovedThreshold = 15 * time.Minute

// NotificationResult holds the result of sending notifications.
type NotificationResult struct {
	Notified int `json:"notified"`
}

// NewManager creates a new purchase manager.
func NewManager(cfg ManagerConfig) *Manager {
	factory := cfg.ProviderFactory
	if factory == nil {
		factory = &provider.DefaultFactory{}
	}

	return &Manager{
		config:          cfg.ConfigStore,
		email:           cfg.EmailSender,
		stsClient:       cfg.STSClient,
		assumeRoleSTS:   cfg.AssumeRoleSTS,
		ambientAWSCreds: cfg.AmbientAWSCreds,
		credStore:       cfg.CredentialStore,
		providerFactory: factory,
		notifyDays:      cfg.NotificationDaysBefore,
		defaults: PurchaseDefaults{
			Term:         cfg.DefaultTerm,
			Payment:      cfg.DefaultPaymentOption,
			Coverage:     cfg.DefaultCoverage,
			RampSchedule: cfg.DefaultRampSchedule,
		},
		dashboardURL:  cfg.DashboardURL,
		oidcSigner:    cfg.OIDCSigner,
		oidcIssuerURL: cfg.OIDCIssuerURL,
	}
}

// finalizeExecution sets the status and completion time on an execution based on the error.
//
// Both partial sentinels (single-account *partialPurchaseError and
// multi-account *multiAccountPartialError) map the root row to
// "partially_completed", never "failed": real commitments exist and a re-approve
// would double-buy them (issues #642 / #1014). errAllAccountsFailed falls
// through to the "failed" default — nothing committed, so a Retry is safe.
func (m *Manager) finalizeExecution(exec *config.PurchaseExecution, execErr error) {
	var partial *partialPurchaseError
	var multiPartial *multiAccountPartialError
	switch {
	case execErr == nil:
		completedAt := time.Now()
		exec.Status = "completed"
		exec.CompletedAt = &completedAt
	case errors.As(execErr, &partial), errors.As(execErr, &multiPartial):
		// At least one rec / account committed a real purchase while others
		// failed. Never mark such a row "failed" — the commitments are real
		// and a re-approve would double-buy them. Record the partial outcome
		// and stamp CompletedAt so the row reads as terminal (the successful
		// recs are done), with the per-rec failures preserved in Error.
		// Append rather than overwrite so any audit-gap note already stamped
		// by aggregatePurchaseOutcomes (a successful rec whose history write
		// failed, issue #621) is not lost.
		completedAt := time.Now()
		exec.Status = "partially_completed"
		exec.Error = appendErrNote(exec.Error, execErr.Error())
		exec.CompletedAt = &completedAt
	default:
		exec.Status = "failed"
		exec.Error = execErr.Error()
	}
}

// claimAndExecute is the single atomic claim-then-execute funnel for the
// non-synchronous executor entry points (SQS execute_purchase and the cron
// ProcessScheduledPurchases sweep), mirroring the guard the synchronous approve
// path already gets from ApproveAndExecute's CAS (issue #1013).
//
// It atomically transitions the row from an executable state
// (approved/pending/notified) to "running" via TransitionExecutionStatus and
// only proceeds when it wins that CAS. A lost CAS (row already claimed by a
// concurrent worker, an overlapping cron tick, an SQS redelivery, or the row
// vanishing mid-flight) is benign: the function returns (claimed=false, nil) and
// the caller should ack/skip without re-running the purchase. A real DB error
// during the claim returns (false, err).
//
// The claim is pinned to the recommendations snapshot the caller gated on: a row
// that was empty when gated must still be empty, and a row with recommendations
// must still have them. A recommendation attach landing between the gate and the
// claim therefore loses the pin, which is a benign skip (the next tick gates the
// row again), instead of running recommendations nobody checked against the cap
// or the notified rule.
//
// For a pending/notified row (the ones the AutoPurchase gate applies to) the
// step cap is re-checked against the claimed row and the current plan; a refusal
// fails the row with the beforeSubmission class, so its suppressions release.
//
// On a won claim it runs executeAndFinalize and returns (true, execErr).
func (m *Manager) claimAndExecute(ctx context.Context, exec *config.PurchaseExecution) (claimed bool, err error) {
	gated := exec.Status == "pending" || exec.Status == "notified"
	gateSawEmpty := len(exec.Recommendations) == 0
	// System-initiated: SQS/cron executor passes nil so transitioned_by = NULL.
	updated, claimErr := m.config.TransitionExecutionStatusPinned(ctx, exec.ExecutionID, []string{"approved", "pending", "notified"}, "running", nil, gateSawEmpty)
	if claimErr != nil {
		if errors.Is(claimErr, config.ErrNotFound) || errors.Is(claimErr, config.ErrExecutionNotInExpectedStatus) {
			// Benign CAS race-loss: another worker/redelivery already owns this
			// row (or it was deleted), or its recommendations changed since the
			// gate. Ack/skip without executing.
			logging.Warnf("Skipping execution %s (CAS claim lost to concurrent worker or changed recommendations): %v", exec.ExecutionID, claimErr)
			return false, nil
		}
		return false, fmt.Errorf("failed to claim execution %s for execution: %w", exec.ExecutionID, claimErr)
	}
	// Carry the committed DB state (status=running, plus any fields refreshed by
	// the RETURNING clause) onto the caller's struct so the rest of the run
	// starts from the claimed row rather than the pre-claim snapshot.
	*exec = *updated
	if gated && len(exec.Recommendations) > 0 {
		if capErr := m.recheckStepCap(ctx, exec); capErr != nil {
			return true, m.failClaimedBeforeSubmission(ctx, exec, capErr)
		}
	}
	return true, m.executeAndFinalize(ctx, exec)
}

// isMultiAccountAckable reports whether a multi-account execErr should be ACKed
// (not redelivered/recounted as a flat failure) because at least one account
// committed a real purchase (issue #1014). A nil error (full success) is also
// ackable. errAllAccountsFailed is NOT ackable — nothing committed, so a
// redelivery is safe and useful.
func isMultiAccountAckable(execErr error) bool {
	if execErr == nil {
		return true
	}
	var partial *multiAccountPartialError
	return errors.As(execErr, &partial)
}

const (
	// The two budgets run back to back (270s), and on AWS Lambda the function
	// timeout ends the invocation whatever these deadlines say, so the Lambda
	// timeout must cover their sum plus a 30s margin (lambda_timeout >= 300,
	// enforced by Terraform validation and TestLambdaTimeoutCoversPurchaseBudget,
	// #913).
	purchaseExecutionTimeout = 4 * time.Minute
	terminalSaveTimeout      = 30 * time.Second
)

// executeAndFinalize runs a purchase and handles status updates, record saving, and progress.
//
// The root execution row is ALWAYS saved with its finalized status — including
// the multi-account fan-out case (issue #1014 / H2). The per-account fan-out
// rows are distinct rows that executeForAccount already saved with their own
// authoritative status; the root row reflects the AGGREGATE outcome
// (completed / partially_completed / failed, classified by finalizeExecution
// from the typed sentinel). The previous `!wasMultiAccount` save-skip left the
// root in whatever pre-execution status it carried, which, now that
// claimAndExecute claims the root to "running" first (issue #1013), would strand
// the root row in "running" until the reaper failed it.
func (m *Manager) executeAndFinalize(ctx context.Context, exec *config.PurchaseExecution) error {
	// A disconnected caller must not cancel a purchase after the claim.
	// Its terminal outcome must survive an exhausted execution budget too.
	execCtx, cancelExec := ctxutil.DetachedWithTimeout(ctx, purchaseExecutionTimeout)
	defer cancelExec()
	// Last line of defense before money moves (issue #1718). Every executor
	// entry point funnels through here, so one check covers all of them.
	execErr := m.preExecutionRefusal(execCtx, exec)
	if execErr == nil {
		execErr = m.executePurchase(execCtx, exec)
	}
	m.finalizeExecution(exec, execErr)
	if execErr != nil {
		logging.Errorf("Failed to execute purchase %s: %v", exec.ExecutionID, execErr)
	}
	saveCtx, cancelSave := ctxutil.DetachedWithTimeout(ctx, terminalSaveTimeout)
	defer cancelSave()
	// Operators hear about every failed or partial row (issue #714), even
	// when the terminal save below fails. A fan-out root is skipped: each
	// per-account row notified for itself in executeForAccount.
	if !isFanOutOutcome(execErr) {
		defer m.notifyExecutionFailure(saveCtx, exec)
	}
	if err := m.saveOrRecoverTerminal(saveCtx, exec, execErr); err != nil {
		logging.Errorf("AUDIT LOSS: failed to save execution status: %v", err)
		// Wrap with ErrAuditLoss regardless of whether executePurchase itself
		// failed. When execErr != nil (provider/partial error), finalizeExecution
		// stamped a terminal status on the in-memory exec struct, but if
		// SavePurchaseExecution then failed the DB row is still in "running" --
		// exactly the stranded-row scenario ErrAuditLoss signals. Preserve the
		// original execErr as the innermost %w so errors.As/errors.Is can still
		// reach it from callers (e.g. claimAndRedrive checking ErrAuditLoss).
		if execErr != nil {
			execErr = fmt.Errorf("%w: terminal save failed (%w); original execution error: %w",
				config.ErrAuditLoss, err, execErr)
		} else {
			execErr = fmt.Errorf("%w: %w", config.ErrAuditLoss, err)
		}
	}
	if execErr == nil {
		if err := m.updatePlanProgress(saveCtx, exec); err != nil {
			m.recordRampAdvanceRefusal(saveCtx, exec, err)
		}
	}
	return execErr
}

// preExecutionRefusal runs every check that must pass before any provider call.
// All refusals except the armed re-drive one are beforeSubmission errors, so
// the row ends failed with no_submission stamped.
func (m *Manager) preExecutionRefusal(ctx context.Context, exec *config.PurchaseExecution) error {
	if err := armedRedriveRefusal(exec); err != nil {
		return err
	}
	if refusal := m.staleAzurePricingRefusal(ctx, exec); refusal != nil {
		return beforeSubmission(refusal)
	}
	if refusal := m.ladderExecutionRefusal(ctx, exec); refusal != nil {
		return beforeSubmission(refusal)
	}
	return nil
}

// beforeSubmissionError marks a failure that happened before any cloud
// purchase call was made for the execution: the code path itself proves no
// commitment can exist, independent of the error text or of provider
// sentinels (the go outcome-unknown sentinel is not pinned yet). It keeps the
// wrapped error's message unchanged.
type beforeSubmissionError struct{ err error }

func (e *beforeSubmissionError) Error() string { return e.err.Error() }
func (e *beforeSubmissionError) Unwrap() error { return e.err }

func beforeSubmission(err error) error { return &beforeSubmissionError{err: err} }

// saveTerminalExecution persists the finalized row. A plan row releases its
// suppressions in the same transaction ONLY when it ended failed and execErr
// proves no purchase call was submitted for any rec (beforeSubmissionError).
// Every other failure keeps them: a lost response ("outcome unknown"), a
// refused armed re-drive or a partial run may still have created a
// commitment, and releasing would let the next step buy it again. Kept
// suppressions expire with the grace period or are released on explicit
// cancel or revoke (platform#631).
func (m *Manager) saveTerminalExecution(ctx context.Context, exec *config.PurchaseExecution, execErr error) error {
	var notSubmitted *beforeSubmissionError
	provenNotSubmitted := exec.Status == "failed" && !anyRecPurchased(exec.Recommendations) && errors.As(execErr, &notSubmitted)
	if provenNotSubmitted {
		// Persist the proof for every row, plan or direct, so a later reader
		// (the ladder reconciler) can tell this failure from a reaped or
		// outcome-unknown one.
		exec.NoSubmission = true
	}
	// Guarded on "running": the executor owns the row only while it holds the
	// claim, so a row the reaper failed or an operator canceled in the meantime
	// is never silently overwritten (issue #829).
	if exec.PlanID == "" || !provenNotSubmitted {
		return m.config.SaveExecutionIfStatus(ctx, exec, executorOwnedStatuses)
	}
	return m.config.WithTx(ctx, func(tx pgx.Tx) error {
		if err := m.config.SaveExecutionIfStatusTx(ctx, tx, exec, executorOwnedStatuses); err != nil {
			return err
		}
		return m.config.DeleteSuppressionsByExecutionTx(ctx, tx, exec.ExecutionID)
	})
}

// recordRampAdvanceRefusal reports a ramp advance that did not happen, stamping
// it onto the execution row that just completed when the outcome is permanent
// so the decision outlives the log retention window.
//
// The refusal paths are new in issue #1669: the previous blind CurrentStep++
// could not decline, so a purchase always moved the ramp. Now money can be spent
// on a step the plan then declines to count, and CompletePlanStep returning
// before its write leaves next_execution_date stale, which shouldNotifyPlan
// reads as daysUntil < 0 and stops notifying that plan. Such a stall is not
// self-correcting, and a logging.Errorf was its only trace.
//
// Two outcomes are deliberately NOT stamped, because both are ordinary and
// neither stays true (issue #1861). A step still waiting on its other accounts
// is the shape of a partially-failed multi-account step being repaired one
// account at a time, and it stops being true the moment the last account buys.
// A step a sibling execution counted moments earlier is the shape of that
// step's last two accounts finishing together, and it means the step WAS
// counted. A note for either would outlive the fact and, worse, would flip a
// cleanly-completed row into History's audit-gap rendering, which keys on a
// non-empty Error and would then tell the operator the purchase's history
// record could not be saved. The waiting state is derived live instead, by
// config.GetStuckRampSteps for the plan-health ramp_blocked factor.
//
// The status stays "completed": the purchase did complete, and only the plan's
// progress accounting did not. Recovery is deliberately not scheduled here; this
// records the fact so History shows it and an operator can act on it.
func (m *Manager) recordRampAdvanceRefusal(ctx context.Context, exec *config.PurchaseExecution, cause error) {
	if errors.Is(cause, config.ErrRampStepIncomplete) || errors.Is(cause, config.ErrRampStepCountedBySibling) {
		logging.Warnf("Plan %s ramp step %d not advanced by execution %s: %v",
			exec.PlanID, exec.StepNumber, exec.ExecutionID, cause)
		return
	}
	logging.Errorf("Failed to update plan progress: %v", cause)
	exec.Error = appendErrNote(exec.Error, fmt.Sprintf("ramp not advanced: %v", cause))
	if saveErr := m.config.SavePurchaseExecution(ctx, exec); saveErr != nil {
		logging.Errorf("AUDIT LOSS: failed to persist ramp-advance refusal for execution %s: %v",
			exec.ExecutionID, saveErr)
	}
}

// allRecsSafeToRedrive reports whether this sweep's automatic in-place re-drive
// may run for exec. Two conditions: every recommendation must be safe to
// re-drive, which RedriveRefusalReason below owns and documents, AND the
// execution must carry at least one recommendation.
//
// The empty-recommendations condition is this sweep's own, deliberately not part
// of the shared safety policy. It is not a duplicate-risk statement: a re-drive
// that purchases nothing cannot double-buy. It means "nothing here worth
// re-driving, hand it to a human", and the safe-fail path it selects is benign
// (the row is marked failed and surfaces in History, where a human can retry
// it). Folding it into the shared predicate would export it to the user-facing
// retry endpoint, where the consequence is the opposite of benign: a permanent
// refusal of a row that cannot double-buy, with no recovery path (issue #1668).
//
// An in-place re-drive keeps the same row, so the lineage key is unchanged and
// DeriveIdempotencyToken(idempotencyLineageKey(exec), i) reproduces the original
// token exactly. That is what lets the provider-side dedupe engage at all.
func allRecsSafeToRedrive(exec *config.PurchaseExecution) bool {
	return len(exec.Recommendations) > 0 && RedriveRefusalReason(exec) == ""
}

// armedRedriveRefusal returns a non-nil error when exec must not be executed
// because it is a RETRY of a purchase that may already have landed, on a
// provider that offers no way to collapse the second attempt onto the first.
//
// Issue #1668 closed this at creation time: the user-facing Retry endpoint
// refuses to build such a successor. A creation-time gate cannot reach
// successors that a PRE-#1668 retry already created, which sit in
// pending/notified/approved/scheduled and buy a second, non-cancelable
// commitment the moment they are approved (issue #1718). This is the
// executor-side backstop for exactly those rows.
//
// It is placed in executeAndFinalize rather than at the individual executors
// because that function is the single funnel every executor reaches money
// through: claimAndExecute (SQS + cron), ApproveAndExecute (token + session
// approve), fireOneDue (the scheduled pre-fire delay sweep) and claimAndRedrive
// (the reaper). One gate at the funnel cannot be missed by a future executor
// the way three copies at three call sites can. executePurchase has exactly one
// production caller, immediately below this check.
//
// The condition is a conjunction and BOTH halves are load-bearing:
//
//   - RetryAttemptN > 0 restricts it to retry successors. A fresh execution
//     (n == 0) is a FIRST purchase with nothing to duplicate, and Azure savings
//     plans must remain buyable, so re-drive safety alone here would block
//     every legitimate first buy.
//   - RedriveRefusalReason != "" restricts it to providers with no duplicate
//     guard, reusing the predicate from #1668 rather than introducing a second
//     notion of re-drive safety.
//
// Returning an error rather than skipping silently means finalizeExecution
// stamps the row "failed" with this reason and executeAndFinalize persists it,
// so an armed row is DEFUSED and visible in History instead of remaining armed
// for the next sweep. Retrying it from the UI then hits the #1668 gate, which
// refuses, so it cannot be re-armed either.
func armedRedriveRefusal(exec *config.PurchaseExecution) error {
	if exec.RetryAttemptN <= 0 {
		return nil
	}
	reason := RedriveRefusalReason(exec)
	if reason == "" {
		return nil
	}
	return fmt.Errorf("refusing to execute retry attempt %d: %s", exec.RetryAttemptN, reason)
}

// RedriveRefusalReason returns a short operator-facing reason why exec must not
// be re-driven, or "" when every recommendation on it carries a provider-side
// guarantee that a second attempt collapses onto the first.
//
// This is the single source of truth for re-drive safety. Both the reaper's
// automatic in-place re-drive (via allRecsSafeToRedrive) and the user-facing
// Retry endpoint (Handler.checkRetryEligibilityGates in internal/api) gate on
// it, so a provider/service that is unsafe for one is unsafe for the other.
// Before issue #1668 only the reaper consulted it, and clicking Retry on a
// landed Azure savings-plans row bought a second savings plan, which cannot be
// canceled.
//
// Safe providers / services (issue #639):
//   - AWS RDS, ElastiCache, MemoryDB, OpenSearch and Savings Plans: a
//     deterministic reservation ID or ClientToken collapses a repeat onto the
//     first attempt (#636/#638).
//   - Azure reservations (compute, relational-db, cache, nosql, memorydb,
//     search, data-warehouse): DoIdempotentPurchaseTwoStep performs a
//     tag-based lookup before purchasing (#729 / #721).
//   - GCP compute (CUDs): server-side RequestId + deterministic name from
//     the token (#654).
//
// NOT safe:
//   - AWS EC2 and Redshift (slugs ec2, compute, redshift, data-warehouse): the
//     purchase APIs take no client token, so a lost response may have bought the
//     commitment and a second attempt would buy another (MON-02). The provider
//     sends the call once and reports "purchase outcome unknown".
//   - Azure savings-plans: the OrderAlias API uses time.Now().UnixNano() as
//     the alias name; there is no server-side idempotency key and no
//     tag-based lookup implemented yet. Re-driving would create a duplicate
//     savings plan.
//   - Any provider this function does not recognize, rather than assuming a
//     guard exists.
//
// Empty provider ("") is treated as AWS (pre-multi-cloud legacy rows).
//
// It answers exactly one question: could re-driving these recommendations buy
// something twice. An execution with no recommendations buys nothing, so it has
// no duplicate risk and gets no refusal here. Callers that need "there is
// nothing worth re-driving" must say so themselves, as allRecsSafeToRedrive
// does above.
//
// The reason is rendered verbatim to the operator, so it explains the refusal
// in product terms rather than naming internals.
func RedriveRefusalReason(exec *config.PurchaseExecution) string {
	for i := range exec.Recommendations {
		if reason := recRedriveRefusalReason(exec.Recommendations[i]); reason != "" {
			return reason
		}
	}
	return ""
}

// recRedriveRefusalReason returns the reason a single recommendation cannot be
// safely re-driven, or "" when it can. Extracted from RedriveRefusalReason to
// keep that function under the gocyclo budget and to make per-rec exclusions
// explicit.
func recRedriveRefusalReason(rec config.RecommendationRecord) string {
	switch rec.Provider {
	case "", "aws":
		// Empty provider is legacy AWS. RDS, ElastiCache, MemoryDB, OpenSearch
		// and Savings Plans collapse a repeat onto the first attempt. EC2 and
		// Redshift cannot: their purchase APIs take no client token or caller
		// ID, and the provider sends them once and reports a lost response as
		// "purchase outcome unknown". The commitment may exist, so a second
		// attempt must not be made until someone has checked the account.
		if awsPurchaseHasNoDuplicateGuard(rec.Service) {
			return "AWS EC2 and Redshift purchases have no provider-side duplicate guard, and a lost response means the earlier attempt may already have bought the commitment. Check EC2 Reserved Instances or Redshift Reserved Nodes in the target account and region; if it is missing, start a new purchase instead of retrying"
		}
		return ""
	case "azure":
		// Azure savings-plans uses a timestamp-based alias name and has no
		// server-side idempotency key, so a re-drive would create a duplicate.
		// All other Azure services use DoIdempotentPurchaseTwoStep (#729).
		if rec.Service == "savingsplans" || rec.Service == "savings-plans" {
			return "Azure savings plans have no provider-side duplicate guard, so re-driving this purchase would buy a second savings plan that cannot be canceled"
		}
		return ""
	case "gcp":
		// GCP compute CUDs use RequestId + deterministic name from the token (#654).
		return ""
	default:
		// Unknown provider: refuse to re-drive rather than risk a double-buy.
		return fmt.Sprintf("provider %q is not known to reject a duplicate purchase, so re-driving this could buy a second commitment", rec.Provider)
	}
}

// awsPurchaseHasNoDuplicateGuard reports whether a recommendation's service
// slug dispatches to the AWS EC2 or Redshift purchase path. The canonical
// slugs (compute, data-warehouse) and the legacy ones (ec2, redshift) reach the
// same client, so all four must be covered or the refusal can be bypassed by
// spelling. Matching goes through mapServiceSlug, the same table the executor
// dispatches on, so the two cannot drift.
func awsPurchaseHasNoDuplicateGuard(service string) bool {
	svc, ok := mapServiceSlug(service)
	if !ok {
		return false
	}
	switch svc {
	case common.ServiceEC2, common.ServiceCompute, common.ServiceRedshift, common.ServiceDataWarehouse:
		return true
	default:
		return false
	}
}

// claimAndRedrive atomically claims a stranded execution (by transitioning its
// status from "approved" to "running") and then re-drives it via
// executeAndFinalize. It is extracted from RecoverStrandedApprovals to keep that
// function's cyclomatic complexity within the gocyclo:10 limit.
//
// It re-drives the row the claim returned, never the caller's pre-claim
// snapshot (issue #1037). The sweep chose the re-drive path from a SELECT, and
// an edit or a concurrent writer can change the row's recommendations, amount
// or account before the claim lands. The re-drive gate (redrivable) is therefore
// re-run on the claimed row; when the claimed row no longer qualifies it is
// failed exactly as safeFail would have failed it had the sweep read it in
// that state (failClaimedRedrive), and nothing is bought.
//
// Returns (true, nil) when the claim was won and the re-drive completed (row is
// now in a terminal state), or when the claimed row was failed because it no
// longer qualifies for a re-drive. A re-drive error is not fatal --
// executeAndFinalize already stamped a terminal status -- so it returns
// (false, nil) on drive failure too (the failed row is still visible in
// History).
// Returns (false, nil) when the CAS claim is lost to a concurrent sweep or the
// row vanishes mid-flight (both are benign races).
// Returns (false, err) on a genuine DB error during the claim step, or when a
// terminal status could not be persisted (ErrAuditLoss).
func (m *Manager) claimAndRedrive(ctx context.Context, exec *config.PurchaseExecution) (bool, error) {
	// Atomically claim ownership before re-driving to prevent two concurrent
	// sweeps (or a late original completion) from both calling executeAndFinalize
	// on the same approved row. The CAS transitions "approved" -> "running";
	// only the winner proceeds.
	// System-initiated: recovery sweep passes nil so transitioned_by = NULL.
	claimed, claimErr := m.claimRunning(ctx, exec.ExecutionID, []string{"approved"}, nil)
	if claimErr != nil {
		// ErrNotFound: row vanished between SELECT and CAS - benign race.
		// ErrExecutionNotInExpectedStatus: another sweep or the original run
		// already claimed/completed this row - also benign.
		if errors.Is(claimErr, config.ErrNotFound) || errors.Is(claimErr, config.ErrExecutionNotInExpectedStatus) {
			logging.Warnf("Skipping re-drive of %s (CAS claim lost to concurrent worker): %v", exec.ExecutionID, claimErr)
			return false, nil
		}
		return false, fmt.Errorf("failed to claim execution %s for re-drive: %w", exec.ExecutionID, claimErr)
	}
	// Carry the whole committed row, not just its status: the purchase below
	// must buy exactly the row this claim owns. The snapshot the sweep listed
	// may predate an edit that landed before the CAS (issue #1037).
	*exec = *claimed
	if !redrivable(exec) {
		return m.failClaimedRedrive(ctx, exec)
	}
	logging.Infof("Recovering stranded execution %s via idempotent re-drive (issue #639)", exec.ExecutionID)
	if driveErr := m.executeAndFinalize(ctx, exec); driveErr != nil {
		logging.Errorf("Re-drive of stranded execution %s failed: %v", exec.ExecutionID, driveErr)
		// Persistence failures (ErrAuditLoss) are non-benign: the row was CAS-ed
		// to "running" but SavePurchaseExecution failed, so no terminal status was
		// persisted. Propagate so the sweep surfaces the error rather than silently
		// dropping a row that is now stranded in "running".
		if errors.Is(driveErr, config.ErrAuditLoss) {
			return false, fmt.Errorf("persistence failure re-driving execution %s (row stranded in running): %w", exec.ExecutionID, driveErr)
		}
		// Benign provider/rec errors: finalizeExecution already stamped a terminal
		// status (failed/partially_completed) and SavePurchaseExecution succeeded.
		// The row is in a terminal state; log and continue without counting as
		// recovered.
		return false, nil
	}
	return true, nil
}

// safeFail atomically transitions a stranded execution to "failed" and stamps a
// recovery error on it. It is extracted from RecoverStrandedApprovals to keep
// that function's cyclomatic complexity within the gocyclo:10 limit.
//
// Returns (true, nil) when the row was successfully transitioned to "failed".
// Returns (false, nil) when TransitionExecutionStatus fails but the row has
// already left "approved" (benign race - the original run completed late;
// not counted as a recovery since no action was taken here).
// Returns (false, err) when a real store failure occurs.
func (m *Manager) safeFail(ctx context.Context, exec *config.PurchaseExecution) (bool, error) {
	logging.Errorf("Recovering stranded approved execution %s (approved but never finalized; failing it for visibility)", exec.ExecutionID)

	// System-initiated: stranded-execution recovery passes nil so transitioned_by = NULL.
	updated, txErr := m.config.TransitionExecutionStatus(ctx, exec.ExecutionID, []string{"approved"}, "failed", nil)
	if txErr != nil {
		// ErrNotFound means the row vanished between the stale SELECT and
		// this CAS attempt (e.g. deleted by an operator or a concurrent
		// sweep already claimed and deleted it). That is a benign race-loss:
		// there is nothing left to fail, and the caller should not be
		// charged with an error.
		if errors.Is(txErr, config.ErrNotFound) {
			logging.Warnf("Skipping recovery of %s (row no longer exists, benign race-loss): %v", exec.ExecutionID, txErr)
			return false, nil
		}
		// ErrExecutionNotInExpectedStatus means the row exists but its
		// status has already moved out of "approved" (a concurrent sweep or
		// the original run won the CAS race). Treat identically to the
		// ErrNotFound case: nothing left to do here, no re-read needed.
		// This mirrors claimAndRedrive and reaper.go, which both treat this
		// sentinel as terminally benign.
		if errors.Is(txErr, config.ErrExecutionNotInExpectedStatus) {
			logging.Warnf("Skipping recovery of %s (row already left approved state, benign CAS race-loss): %v", exec.ExecutionID, txErr)
			return false, nil
		}
		// Distinguish benign races (row already left the "approved"
		// state - concurrent sweep handled it, or the original run
		// finished after the LIST snapshot) from real store
		// failures (DB unreachable, query syntax error). A real
		// store failure must fail the sweep so a transient DB
		// outage does not silently under-recover. We probe the
		// current row state via GetExecutionByID: a clean read
		// with Status != "approved" confirms the race; any other
		// outcome (read error, still-approved row) is a real
		// failure worth propagating.
		current, getErr := m.config.GetExecutionByID(ctx, exec.ExecutionID)
		if getErr == nil && current != nil && current.Status != "approved" {
			logging.Warnf("Skipping recovery of %s (already transitioned out of approved): %v", exec.ExecutionID, txErr)
			return false, nil
		}
		return false, fmt.Errorf("failed to transition stranded execution %s to failed: %w", exec.ExecutionID, txErr)
	}

	updated.Error = strandedRecoveryError(exec)
	if saveErr := m.config.SavePurchaseExecution(ctx, updated); saveErr != nil {
		// The atomic flip to "failed" already landed via TransitionExecutionStatus;
		// only the explanatory error string failed to persist. Log loudly but
		// still count the recovery - the row is no longer stranded in "approved".
		logging.Errorf("AUDIT GAP: failed to stamp recovery error on %s: %v", exec.ExecutionID, saveErr)
	}
	return true, nil
}

// RecoverStrandedApprovals finds executions stuck in the "approved" status past
// staleApprovedThreshold and either re-drives them idempotently (executions
// where every rec is safe to re-drive and the row has a durable ExecutionID)
// or drives them into a terminal "failed" state (rows with unsafe recs or
// without a stable ExecutionID).
//
// Idempotent re-drive path (issue #639): all AWS, Azure reservations, and GCP
// compute service clients derive or look up a deterministic idempotency key
// from DeriveIdempotencyToken(idempotencyLineageKey(exec), i). This is an
// IN-PLACE re-drive — the same row, so the lineage key is unchanged — which
// reproduces the same token, so the cloud provider dedupes the second call and
// no double-purchase occurs. (The retry/fan-out double-buy hole #1012 closes is
// about NEW rows minting a fresh ExecutionID; that does not apply here.) The row
// transitions directly to "completed"
// (or "failed"/"partially_completed" on a genuine error), bypassing the manual
// Retry step required by the old safe-fail path. See allRecsSafeToRedrive for
// which provider/service combinations are eligible.
//
// Safe-fail path: Azure savings-plans recs are excluded because the OrderAlias
// API uses a timestamp-based alias name with no idempotency key. Executions
// without a stable ExecutionID (legacy rows) also fall through because
// idempotencyLineageKey(exec) falls back to "" for them and
// DeriveIdempotencyToken("", i) would produce the same token set for every
// such row. These fall through to the original behavior: the row is atomically
// transitioned to "failed" so it surfaces in History and can be Retry-ed by
// an operator after confirming the cloud-side state.
//
// The transition in the safe-fail path is atomic: TransitionExecutionStatus only
// flips rows still in "approved", so if the original run finally completes between
// the stale SELECT and this UPDATE, the transition is a no-op and the genuine
// "completed" status is preserved.
func (m *Manager) RecoverStrandedApprovals(ctx context.Context) (int, error) {
	stranded, err := m.config.GetStaleApprovedExecutions(ctx, staleApprovedThreshold)
	if err != nil {
		return 0, fmt.Errorf("failed to list stranded approved executions: %w", err)
	}

	recovered := 0
	for i := range stranded {
		exec := &stranded[i]

		// Idempotent re-drive path (issue #639): all recs honor
		// opts.IdempotencyToken via DeriveIdempotencyToken(idempotencyLineageKey(exec), i),
		// so a second in-place call on the same row is a safe no-op on the
		// provider side. The ExecutionID must be non-empty so the lineage key
		// (or its ExecutionID fallback for legacy rows) is unique; an empty ID
		// would map every legacy row to the same token set.
		if redrivable(exec) {
			counted, driveErr := m.claimAndRedrive(ctx, exec)
			if driveErr != nil {
				return recovered, driveErr
			}
			if counted {
				recovered++
			}
			continue
		}

		// Safe-fail path for mixed/Azure/GCP/legacy executions.
		counted, failErr := m.safeFail(ctx, exec)
		if failErr != nil {
			return recovered, failErr
		}
		if counted {
			recovered++
		}
	}

	return recovered, nil
}

// executableByScheduler reports whether a pending or notified execution may be
// auto-executed by the cron sweep or an SQS execute_purchase message, without
// an explicit human approval action (fail closed on money paths).
//
// Rules:
//   - web-submitted rows (Source == common.PurchaseSourceWeb, "cudly-web") must
//     wait for the token-link approval path; the scheduler and SQS paths must
//     never bypass that gate.
//   - All other pending/notified rows require the owning plan to have
//     AutoPurchase=true. A plan-fetch error is propagated so the caller can
//     fail closed rather than defaulting to "execute".
//   - The global kill switch (GlobalConfig.AutoPurchaseEnabled) must be on. A
//     read error is propagated. Only AutoPurchase plans reach this check, so
//     manual purchases are unaffected.
//   - A recommendation-bearing row must be "notified" (the notification email is
//     the human veto window; a "pending" row may never have been emailed) and
//     must fit the plan's max_step_commitment (CheckStepCommitmentCap, whose
//     refusal is returned as an error). Rows without recommendations are left
//     to the no-recommendations refusal in executePurchase.
//
// "approved" rows are handled by the session/token approval paths and
// RecoverStrandedApprovals; this helper is only called for pending/notified.
func (m *Manager) executableByScheduler(ctx context.Context, exec *config.PurchaseExecution) (bool, error) {
	// Compare against the typed source constant, not a bare "web" literal: the
	// persisted value is "cudly-web" (common.PurchaseSourceWeb), so the old
	// literal never matched and web rows could be auto-executed without the
	// token-link approval when AutoPurchase=true (fail-open on a money path).
	if exec.Source == common.PurchaseSourceWeb {
		return false, nil
	}
	plan, err := m.config.GetPurchasePlan(ctx, exec.PlanID)
	if err != nil {
		return false, fmt.Errorf("failed to fetch plan %s for AutoPurchase gate: %w", exec.PlanID, err)
	}
	if !plan.AutoPurchase {
		return false, nil
	}
	enabled, err := m.autoPurchaseEnabled(ctx)
	if err != nil {
		return false, err
	}
	if !enabled {
		return false, nil
	}
	if len(exec.Recommendations) == 0 {
		return true, nil
	}
	if exec.Status != "notified" {
		return false, nil
	}
	if err := CheckStepCommitmentCap(plan, exec.Recommendations); err != nil {
		return false, err
	}
	return true, nil
}

// autoPurchaseEnabled reads the global auto-purchase kill switch. It fails
// closed: a read error is returned, never read as "enabled".
func (m *Manager) autoPurchaseEnabled(ctx context.Context) (bool, error) {
	cfg, err := m.config.GetGlobalConfig(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to read global config for the auto-purchase kill switch: %w", err)
	}
	if cfg == nil {
		return false, errors.New("global config unavailable for the auto-purchase kill switch")
	}
	return cfg.AutoPurchaseEnabled, nil
}

// processOneExecution runs the full gate+claim+execute pipeline for a single
// due pending/notified execution. It updates the Processed/Executed/Failed
// counters and Errors slice on the supplied ProcessResult in place.
// Extracted from ProcessScheduledPurchases to keep that function under the
// gocyclo:10 threshold.
func (m *Manager) processOneExecution(ctx context.Context, exec config.PurchaseExecution, result *ProcessResult) {
	// AutoPurchase gate: pending/notified rows are only eligible for
	// automatic execution when the owning plan has AutoPurchase=true AND
	// the row was not web-submitted (those must go through the token-link
	// path). Fail closed: a plan-fetch error counts as a failure rather
	// than defaulting to "execute" (no silent money action on error).
	eligible, gateErr := m.executableByScheduler(ctx, &exec)
	if gateErr != nil {
		result.Failed++
		result.Errors = append(result.Errors, fmt.Sprintf("%s: AutoPurchase gate check failed: %v", exec.ExecutionID, gateErr))
		return
	}
	if !eligible {
		logging.Infof("Skipping execution %s (AutoPurchase=false, kill switch off, source=web, or recommendation-bearing row not yet notified; requires explicit approval)", exec.ExecutionID)
		return
	}

	result.Processed++
	logging.Infof("Executing scheduled purchase: %s", exec.ExecutionID)

	// Atomically claim the row before executing (issue #1013). Overlapping
	// cron ticks (a tick that runs longer than the interval, EventBridge
	// duplicate/overlapping deliveries, or cron racing the SQS path) would
	// otherwise both execute the same due row. claimAndExecute CASes the row
	// to "running" and only the winner runs; a lost claim is skipped without
	// re-executing.
	claimed, execErr := m.claimAndExecute(ctx, &exec)
	if !claimed {
		// execErr != nil here is a real DB error during the claim (count as
		// failed); execErr == nil is a benign CAS race-loss (skip silently).
		if execErr != nil {
			result.Failed++
			result.Errors = append(result.Errors, fmt.Sprintf("%s: claim failed: %v", exec.ExecutionID, execErr))
		}
		return
	}

	// A multi-account run where at least one account committed is a success
	// for ack purposes (issue #1014): the per-account rows own the truth and
	// re-running would double-buy. Only a genuine failure (nothing
	// committed) is counted/surfaced.
	if isMultiAccountAckable(execErr) {
		result.Executed++
		return
	}
	result.Failed++
	result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", exec.ExecutionID, execErr))
}

// ProcessScheduledPurchases checks for and executes scheduled purchases.
func (m *Manager) ProcessScheduledPurchases(ctx context.Context) (*ProcessResult, error) {
	logging.Info("Processing scheduled purchases...")

	// Recover any executions stranded in "approved" by an interrupted
	// synchronous run before processing fresh pending work (issue #632).
	recovered, err := m.RecoverStrandedApprovals(ctx)
	if err != nil {
		// A recovery failure must not block scheduled purchases — log and continue
		// with the pending-execution pass; the next tick retries the sweep.
		logging.Errorf("Failed to recover stranded approved executions: %v", err)
	}

	// Get all pending executions
	executions, err := m.config.GetPendingExecutions(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get pending executions: %w", err)
	}

	now := time.Now()
	result := &ProcessResult{Recovered: recovered}

	for i := range executions {
		exec := executions[i]
		// Check if it's time to execute
		if exec.ScheduledDate.After(now) {
			logging.Debugf("Execution %s not yet due (scheduled for %s)", exec.ExecutionID, exec.ScheduledDate)
			continue
		}

		// Positive allowlist (issue #1013 / M2): only the two executable
		// pre-purchase states proceed. The query already filters to
		// pending/notified, but a row another worker transitioned in the gap
		// between SELECT and here (approved/running/failed/...) must be skipped.
		// The atomic claim below is the real guard; this is defense-in-depth.
		if exec.Status != "pending" && exec.Status != "notified" {
			continue
		}

		m.processOneExecution(ctx, exec, result)
	}

	return result, nil
}
