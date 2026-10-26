// Package api provides the HTTP API handlers for the CUDly dashboard.
package api

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
	"github.com/aws/aws-lambda-go/events"
	"github.com/jackc/pgx/v5"
)

// History handlers.
func (h *Handler) getHistory(ctx context.Context, req *events.LambdaFunctionURLRequest, params map[string]string) (any, error) {
	// Purchase history can leak across accounts — gate on view:purchases AND
	// filter the returned records by the session's allowed_accounts list.
	session, err := h.requirePermission(ctx, req, "view", "purchases")
	if err != nil {
		return nil, err
	}

	filters, err := parseHistoryFilters(params)
	if err != nil {
		return nil, err
	}

	// Resolve the requested account identifiers to the dual-column filter
	// inputs (UUIDs + their external account numbers) so both the SQL path and
	// the in-memory execution path match rows that carry only one of the two
	// representations (issue #701/#498/#866). The legacy singular account_id
	// (a top-bar chip UUID, or a raw external number for pre-UUID callers) is
	// folded in here so fetchPurchaseHistory sees a single unified account set.
	h.resolveHistoryAccountFilter(ctx, &filters)

	// The paged stream: completed purchases and terminal executions, newest
	// first, filters.Limit rows the caller may see, starting after
	// filters.Cursor (issue #674, see handler_history_page.go).
	page, err := h.fetchHistoryPage(ctx, session, &filters)
	if err != nil {
		return nil, err
	}

	// The account scope is resolved once per request and applied to the
	// actionable rows and the summary (issue #675). The paged stream applies
	// the same rule per batch (markOutOfScope).
	scope, err := h.loadHistoryScope(ctx, session)
	if err != nil {
		return nil, err
	}

	// Pull the actionable executions so the user can see (and cancel) their
	// own in-flight approvals without waiting for the email. Executions live
	// in a separate table (purchase_executions) from the completed
	// purchase_history rows, so we merge after the fact. They are not paged:
	// the first page lists the whole class (up to its own cap) whatever the
	// page size, and later pages leave it out, so paging can never push a
	// pending approval off the first page. A failure to list them must not
	// hide completed history — log, skip, continue, and flag the response.
	// The same filter set and the account scope are pushed into the SQL read
	// so the class cap counts only rows the caller can be shown (issue #632),
	// and re-checked in memory against the synthesized row so the two halves
	// of the merged response are consistently scoped (issue #701).
	//
	// Stale pending/notified executions are expired AFTER the response is
	// assembled so the GET handler is a pure read (issue #1032): the caller
	// sees current DB state and the next History load reflects the updated
	// status. On servers the transitions fire in a goroutine; on Lambda they
	// run synchronously before returning because the execution environment
	// freezes once the response is out (issue #1170). The sweep is deferred so
	// it also runs after the summary aggregate below: on Lambda a synchronous
	// sweep before it would make the cards count as "expired" the rows the
	// list still shows as pending (issue #675). Pages fetched with a cursor
	// do not list the actionable class, so they do not sweep it either.
	var extra []config.PurchaseHistoryRecord
	var staleExecs []config.PurchaseExecution
	actionableIncomplete := false
	if filters.Cursor == nil {
		extra, staleExecs, actionableIncomplete = h.fetchExecutionsAsHistory(ctx, filters, scope)
		extra = scope.filter(extra)
	}
	defer h.expireStaleExecutions(staleExecs)

	all := make([]config.PurchaseHistoryRecord, 0, len(page.rows)+len(extra))
	all = append(all, page.rows...)
	all = append(all, extra...)

	// Newest first across both sources so pending items don't get buried at
	// the bottom just because their synthetic records were appended last.
	// The stream rows are already in order and the sort is stable, so their
	// relative order (and the cursor's position) is unchanged.
	sort.SliceStable(all, func(i, j int) bool {
		return all[i].Timestamp.After(all[j].Timestamp)
	})

	// The summary covers every row matching the filters and the caller's
	// account scope, not just this page (issue #675), so it is the same on
	// every page. It falls back to the page when the aggregate query fails,
	// and SummaryComplete says which.
	summary, summaryComplete := h.historySummary(ctx, filters, scope, all)

	// Truncated is a single boolean: a count would reveal other tenants' row
	// volume to a scoped user. has_more is judged on the rows the caller may
	// see, so it is never set by rows outside the caller's scope.
	return HistoryResponse{
		Summary:                   summary,
		SummaryComplete:           summaryComplete,
		Purchases:                 all,
		Truncated:                 page.nextCursor != "" || page.incomplete || actionableIncomplete,
		Limit:                     filters.Limit,
		ExecutionsLimit:           filters.Limit,
		ActionableExecutionsLimit: config.MaxListLimit,
		HasMore:                   page.nextCursor != "",
		NextCursor:                page.nextCursor,
	}, nil
}

// actionableExecutionStatuses are the PurchaseExecution statuses a user can still
// act on or that are in flight. They are fetched separately from the terminal
// class so newer terminal rows cannot push them out of the capped listing, and
// they are not paged: the first page lists them all (issue #674).
//
// Issue #372 assumed approval executes synchronously (approved -> completed/failed inside one
// HTTP request) and so excluded "approved"; issue #621 showed that
// assumption breaks under interruption: a Lambda timeout or a crash mid-
// execution leaves the row stuck in "approved"/"running"/"paused", in
// neither purchase_history nor this list, and it silently vanishes — the
// worst failure mode for a financial action (the user can't tell whether
// the purchase fired and may re-approve). So we now load those in-flight
// states too, rendered with a clear "in progress" badge rather than as a
// (misleading) completed row.
//
// "scheduled" is included so Gmail-style pre-fire delayed executions (issue #291
// wave-2) appear in the History view with a Revoke button before the cloud SDK
// call fires. Without this entry the row is invisible to the History UI, making
// the Revoke button unreachable (issue #290, second-wave CR Finding E).
var actionableExecutionStatuses = []string{"pending", "notified", "scheduled", "approved", "running", "paused"}

// terminalExecutionStatuses are the finished PurchaseExecution statuses the
// History view pages through alongside completed purchases (fetchHistoryPage).
//
// "completed" is also loaded, but the stream synthesizes a
// row for it ONLY when the execution carries a non-empty Error — the
// audit-gap case where the purchase succeeded but its purchase_history
// write failed (issue #621 secondary path). A normal completed execution
// has Error=="" and is skipped here so it surfaces exactly once via its
// purchase_history rows (no duplicate). The execution row's PurchaseID is
// the ExecutionID while a purchase_history row's is the CommitmentID, so
// the keys never collide even when both happen to render.
//
// "partially_completed" (issue #642) is loaded and ALWAYS synthesized: a
// partial run committed some recs to purchase_history (those render from the
// DB rows) and failed others. The synthesized execution row carries the
// partial-failure marker and is flagged IsAuditGap so its execution-level
// dollars are excluded from the dashboard totals — the committed dollars are
// already counted via the per-rec purchase_history rows that succeeded.
// Both the US-spelling status (config.StatusCanceled) and the legacy British
// spelling (config.LegacyStatusCanceled) are listed: during the expand-contract
// rename (migration 000089) old code may still write the legacy value before
// the rolling deploy completes. The contract migration (#1278) normalizes the
// data once the deploy is verified stable; drop the legacy entry here then.
var terminalExecutionStatuses = []string{"completed", "partially_completed", "failed", "expired", config.StatusCanceled, config.LegacyStatusCanceled}

// approvalExpiryWindow is how long a pending approval stays actionable
// before the History view flips it to "expired". Aligns with the
// cleanup-executions TTL and the user expectation that a week-old
// approval link is stale.
const approvalExpiryWindow = 7 * 24 * time.Hour

// fetchExecutionsAsHistory returns the actionable PurchaseExecutions as
// synthetic PurchaseHistoryRecord entries so the /api/history response is a
// single flat list. Execution-level aggregates (TotalUpfrontCost /
// EstimatedSavings) become the row's cost fields; provider/service collapse
// to "multiple" because a single execution can span providers. The approver
// address is looked up from global config once and attached to pending/
// notified rows so the UI can tell the user exactly whose inbox holds the
// approval link. Stale pending/notified executions are expired after this
// call returns (see expireStaleExecutions) so the GET is a pure read:
// the response rows carry the pre-transition status and the next request
// sees the updated state. A listing error is logged and skipped — completed
// history must still render. Terminal executions are read by the paged
// stream instead (fetchHistoryPage).
//
// The filter set (issue #701) and the caller's account scope are pushed into
// the SQL read as a superset (issue #632), so the MaxListLimit cap counts only
// executions the caller could be shown and other tenants' or filtered-out rows
// cannot crowd a pending approval out. The exact checks still run in Go
// against the synthetic row: provider via the recs' providers, account via
// CloudAccountID, plan via PlanID, date via ScheduledDate here, and the scope
// in the caller.
//
// The third return value reports that actionable executions may be missing:
// the class reached its MaxListLimit cap, or its listing failed (issue #686).
func (h *Handler) fetchExecutionsAsHistory(ctx context.Context, filters historyFilters, scope historyScope) ([]config.PurchaseHistoryRecord, []config.PurchaseExecution, bool) {
	q := actionableExecutionsQuery(&filters, &scope)
	executions, ok := h.fetchExecutionsByClass(ctx, q)
	incomplete := !ok || len(executions) >= config.MaxListLimit
	staleExecs := h.staleExecutionsToSweep(ctx, &q, executions)
	if len(executions) == 0 {
		return nil, staleExecs, incomplete
	}
	approver := h.resolvePendingApproverEmail(ctx)
	userEmailCache := h.resolveUserEmails(ctx, executions)
	out := make([]config.PurchaseHistoryRecord, 0, len(executions))
	for _rvc := range executions {
		exec := executions[_rvc]
		if !filters.matchesExecution(exec) {
			continue
		}
		out = append(out, executionToHistoryRow(exec, approver, createdByEmail(userEmailCache, &exec)))
	}
	return out, staleExecs, incomplete
}

// fetchExecutionsByClass lists one status class. A listing error is logged and
// the class skipped (ok=false) so the other class and completed history still
// render; the caller flags the response as incomplete so the hidden rows are
// not silent (issue #686).
//
// q carries the request filters, plan_id included, and the caller's scope, so
// a plan view or a scoped caller is not crowded out by other rows under the
// limit (issues #632, #1404).
func (h *Handler) fetchExecutionsByClass(ctx context.Context, q config.ExecutionsByStatusesQuery) ([]config.PurchaseExecution, bool) {
	rows, err := h.config.GetExecutionsByStatusesFiltered(ctx, q)
	if err != nil {
		logging.Warnf("history: failed to load executions with statuses %v: %v", q.Statuses, err)
		return nil, false
	}
	return rows, true
}

// isStaleExecution reports whether the execution is a pending/notified
// approval older than approvalExpiryWindow that should be transitioned to
// "expired". Extracted so both fetchExecutionsAsHistory and the expire
// sweep (sync on Lambda, async on servers) share one staleness check.
func isStaleExecution(exec config.PurchaseExecution) bool {
	if exec.Status != "pending" && exec.Status != "notified" {
		return false
	}
	return time.Since(exec.ScheduledDate) >= approvalExpiryWindow
}

// expireStaleExecutions fires TransitionExecutionStatus for each stale
// execution. On long-running servers this happens in a best-effort goroutine
// that outlives the request context: context.Background() ensures the
// transitions are not canceled when the HTTP handler returns. On Lambda that
// guarantee does not hold: the execution environment freezes as soon as the
// response is returned, so a background goroutine would be suspended
// mid-sweep and stale rows could stay "pending" indefinitely (issue #1170).
// There the sweep runs synchronously before the handler returns, mirroring
// the SWR cache's isLambda gate (ri_utilization_cache.go) and reading the same
// h.isLambda value (HandlerConfig.IsLambda, the Application's single captured
// answer, issue #110) rather than the live environment. The sweep is a handful
// of cheap UPDATEs, so the synchronous cost on Lambda is negligible. Errors are
// logged and skipped — a missed transition leaves the row "pending" until the
// next History load, which is better than failing the read response.
//
// The sweep is idempotent per execution ID: TransitionExecutionStatus is
// guarded by the FROM-status list ("pending","notified"), so a concurrent
// caller that wins the race causes the loser's update to affect 0 rows and
// return an error, which is already handled by the Warnf below. Two
// simultaneous GET requests can both sweep the same stale row; only one
// transition commits — this is safe and expected.
func (h *Handler) expireStaleExecutions(staleExecs []config.PurchaseExecution) {
	if len(staleExecs) == 0 {
		return
	}
	if len(staleExecs) > config.DefaultListLimit {
		logging.Warnf("history: sweeping %d of %d stale executions", config.DefaultListLimit, len(staleExecs))
		staleExecs = staleExecs[:config.DefaultListLimit]
	}
	if h.isLambda {
		h.expireStaleExecutionsSweep(staleExecs)
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logging.Warnf("history: expireStaleExecutionsAsync goroutine panic: %v", r)
			}
		}()
		h.expireStaleExecutionsSweep(staleExecs)
	}()
}

// expireStaleExecutionsSweep is the shared sweep body for both branches of
// expireStaleExecutions. It deliberately uses context.Background()
// rather than the request context: on servers the goroutine outlives the
// request, and on Lambda the request context may carry a deadline that
// should not abort the best-effort transitions.
func (h *Handler) expireStaleExecutionsSweep(staleExecs []config.PurchaseExecution) {
	ctx := context.Background()
	for i := range staleExecs {
		// The suppressions go in the same transaction as the status flip: an
		// expired step bought nothing, and leaving its rows would keep hiding
		// recommendations nobody purchased (platform#631).
		err := h.config.WithTx(ctx, func(tx pgx.Tx) error {
			expired, expErr := h.config.ExpireExecutionAtomic(ctx, tx, staleExecs[i].ExecutionID)
			if expErr != nil || !expired {
				return expErr
			}
			return h.config.DeleteSuppressionsByExecutionTx(ctx, tx, staleExecs[i].ExecutionID)
		})
		if err != nil {
			logging.Warnf("history: expire sweep for execution %s failed: %v", staleExecs[i].ExecutionID, err)
		}
	}
}

// resolveUserEmails builds a map of user-ID to email by calling GetUser once
// per unique non-nil CreatedByUserID found in the execution list. Lookup
// failures are logged and skipped — a missing email degrades gracefully (the
// UI falls back to the raw UUID via created_by_user_id). Called once per
// /api/history request so the cost is proportional to the number of distinct
// creators, not the number of execution rows.
func (h *Handler) resolveUserEmails(ctx context.Context, executions []config.PurchaseExecution) map[string]string {
	seen := make(map[string]struct{})
	for _rvc := range executions {
		exec := executions[_rvc]
		if exec.CreatedByUserID != nil && *exec.CreatedByUserID != "" {
			seen[*exec.CreatedByUserID] = struct{}{}
		}
	}
	out := make(map[string]string, len(seen))
	for uid := range seen {
		user, err := h.auth.GetUser(ctx, uid)
		if err != nil {
			logging.Warnf("history: failed to resolve email for user %s: %v", uid, err)
			continue
		}
		out[uid] = user.Email
	}
	return out
}

// resolvePendingApproverEmail returns the notification email the approval
// link was sent to (or would have been, if SES failed). Single-tenant
// deployments share one value across every pending row, so this is looked up
// once per /api/history call. A lookup error or an empty address returns a
// sentinel so the UI still renders the pending row — just without an
// addressee.
func (h *Handler) resolvePendingApproverEmail(ctx context.Context) string {
	globalCfg, cfgErr := h.config.GetGlobalConfig(ctx)
	if cfgErr != nil {
		logging.Warnf("history: failed to resolve approver email for pending rows: %v", cfgErr)
		return "(notification email not set)"
	}
	if globalCfg.NotificationEmail == nil || *globalCfg.NotificationEmail == "" {
		return "(notification email not set)"
	}
	return *globalCfg.NotificationEmail
}

// executionToHistoryRow projects any non-completed PurchaseExecution into
// the flat history-row shape. Status maps 1:1 onto the history Status field.
// For pending/notified rows we attach the approver email so the UI can show
// "awaiting approval from X"; for failed rows we surface the stored Error
// message as the status description so the user sees WHY it failed (e.g.
// "send failed: Missing domain"). createdByEmail is the resolved email for
// the execution's creator (empty when not resolvable).
func executionToHistoryRow(exec config.PurchaseExecution, approver, createdByEmail string) config.PurchaseHistoryRecord {
	var accountID string
	if exec.CloudAccountID != nil {
		accountID = *exec.CloudAccountID
	} else {
		// Web-initiated bulk-purchase executions (handler_purchases.go's
		// buildPendingExecution) never populate exec.CloudAccountID — the
		// per-rec CloudAccountID is the canonical source. Fall back to that
		// so the Approval queue's Account cell shows the actual account ID
		// instead of "-". Returns "" when recs disagree (a basket that
		// genuinely spans accounts honestly renders as the dash fallback
		// rather than a misleading single account).
		accountID = collapseRecommendationAccount(exec.Recommendations)
	}
	var createdBy string
	if exec.CreatedByUserID != nil {
		createdBy = *exec.CreatedByUserID
	}
	var retryExecID string
	if exec.RetryExecutionID != nil {
		retryExecID = *exec.RetryExecutionID
	}
	row := config.PurchaseHistoryRecord{
		AccountID:          accountID,
		PurchaseID:         exec.ExecutionID,
		Timestamp:          exec.ScheduledDate,
		Provider:           collapseRecommendationProvider(exec.Recommendations),
		Count:              len(exec.Recommendations),
		PlanID:             exec.PlanID,
		Status:             exec.Status,
		CreatedByUserID:    createdBy,
		CreatedBySystem:    exec.CreatedBySystem,
		CreatedByUserEmail: createdByEmail,
		RetryExecutionID:   retryExecID,
		RetryAttemptN:      exec.RetryAttemptN,
	}
	projectRecommendationFields(&row, exec)
	// Compute ops_hint at read time (issue #47, Q3) so updates to the
	// persistent-failure map land instantly without a re-collect. Only
	// failed rows can carry a hint — the resolver returns "" for empty
	// inputs, so this is safe to call unconditionally.
	if exec.Status == "failed" {
		row.OpsHint = resolveOpsHint(exec.Error)
		// A re-drive the backend will always refuse is permanent, so it wins
		// over the operator-fixable failure-reason hint (which promises that
		// fixing the config makes Retry work). Same call and input as the
		// retry handler's gate so the two cannot disagree (issue #161).
		if reason := purchase.RedriveRefusalReason(&exec); reason != "" {
			row.OpsHint = reason
			row.RedriveUnsafe = true
		}
	}
	annotateHistoryRowByStatus(&row, exec, approver)
	return row
}

// annotateHistoryRowByStatus fills in Approver + StatusDescription on the
// row based on exec.Status. Split from executionToHistoryRow to keep the
// switch below under the gocyclo threshold.
func annotateHistoryRowByStatus(row *config.PurchaseHistoryRecord, exec config.PurchaseExecution, approver string) {
	switch exec.Status {
	case "pending", "notified":
		row.Approver = approver
	case "failed":
		row.StatusDescription = exec.Error
	case "expired":
		row.StatusDescription = "approval link expired (not approved within 7 days)"
	case config.StatusCanceled, config.LegacyStatusCanceled:
		// The legacy British spelling is still matched during the
		// expand-contract rename (migration 000089) until the contract
		// migration (#1278) normalizes and drops it.
		annotateCancelled(row, exec, approver)
	default:
		// In-flight (approved/running/scheduled/paused) and audit-gap
		// (partially_completed/completed) cases. Split out to keep this switch
		// under the cyclomatic-complexity limit.
		annotateInFlightOrAuditGapRow(row, exec, approver)
	}
}

// annotateInFlightOrAuditGapRow handles the non-terminal and audit-gap statuses
// for annotateHistoryRowByStatus: approved/running, scheduled, paused,
// partially_completed, and completed (audit-gap). Extracted to keep the parent
// switch under the cyclomatic-complexity limit.
func annotateInFlightOrAuditGapRow(row *config.PurchaseHistoryRecord, exec config.PurchaseExecution, approver string) {
	switch exec.Status {
	case "approved", "running":
		// In-flight (issue #621): approved/running rows are NOT terminal —
		// the synchronous AWS purchase is mid-execution or got interrupted
		// (Lambda timeout / crash). Resolve who approved (so the user knows
		// who to ask) and overlay an "in progress" note so the UI never
		// renders this as a finished purchase.
		annotateApproved(row, exec, approver)
		row.StatusDescription = "approved — purchase in progress"
	case "scheduled":
		// Gmail-style pre-fire delay (issue #291 wave-2): the cloud SDK has not
		// been called yet. The "revocation window" for the frontend Revoke button
		// is the time until scheduled_execution_at (after which the scheduler
		// fires the SDK call and the row transitions to approved/running). Populate
		// RevocationWindowClosesAt with the fire time so canRevokeCompletedRow can
		// use its standard window check (issue #290, second-wave CR Finding E).
		if exec.ScheduledExecutionAt != nil {
			t := *exec.ScheduledExecutionAt
			row.RevocationWindowClosesAt = &t
		}
		row.StatusDescription = "scheduled — revoke before execution window closes to cancel for free"
	case "paused":
		row.Approver = approver
		row.StatusDescription = "purchase paused — resume or cancel from the plan"
	case "partially_completed":
		// #642: some recs committed, some failed. The committed recs are
		// surfaced via their own purchase_history rows; this synthesized row
		// is the audit flag for the failures. Flag IsAuditGap so the dashboard
		// excludes its execution-level dollars (the committed dollars are
		// counted on the per-rec purchase_history rows, not here) — same
		// double-count guard as the audit-gap completed case below.
		row.IsAuditGap = true
		annotateApproved(row, exec, approver)
		row.StatusDescription = "partially completed — some commitments succeeded, others failed: " + exec.Error
	case "completed":
		// Only audit-gap completed executions reach here (fetchExecutionsAsHistory
		// skips clean completed rows). exec.Error carries why the history write
		// failed after a successful purchase — surface it so the purchase is
		// visible despite the missing purchase_history row (issue #621). Flag the
		// row so the dollar accounting excludes its execution-level total without
		// relying on StatusDescription being set (which annotateApproved may also
		// populate).
		row.IsAuditGap = true
		annotateApproved(row, exec, approver)
		if exec.Error != "" {
			row.StatusDescription = "purchase completed but its history record could not be saved: " + exec.Error
		}
	}
}

// annotateCancelled resolves who canceled the execution:
//  1. exec.CancelledBy — populated by the session-authed deep-link flow;
//     exact session-authed click attribution.
//  2. approver — the notification inbox that received the cancel token;
//     authoritative accountable party but not necessarily the clicker.
//     Used on legacy token-only paths (async workers, old email clicks).
func annotateCancelled(row *config.PurchaseHistoryRecord, exec config.PurchaseExecution, approver string) {
	if exec.CancelledBy != nil && *exec.CancelledBy != "" {
		row.Approver = *exec.CancelledBy
		row.StatusDescription = "canceled by " + *exec.CancelledBy
		return
	}
	if approver != "" {
		row.Approver = approver
		row.StatusDescription = "canceled by " + approver + " (via approval link)"
		return
	}
	row.StatusDescription = "canceled via approval link"
}

// annotateApproved resolves who approved the execution using the same
// two-tier lookup as annotateCancelled — session-authed click wins,
// notification inbox fills in otherwise.
func annotateApproved(row *config.PurchaseHistoryRecord, exec config.PurchaseExecution, approver string) {
	if exec.ApprovedBy != nil && *exec.ApprovedBy != "" {
		row.Approver = *exec.ApprovedBy
		row.StatusDescription = "approved by " + *exec.ApprovedBy
		return
	}
	if approver != "" {
		row.Approver = approver
	}
}

// projectRecommendationFields fills the row's resource/cost/term columns from
// the execution's recommendations so an in-progress (approved/running/paused)
// or audit-gap row renders with the SAME shape the completed purchase_history
// row would (issue #631). The completed row carries per-commitment service /
// resource_type / region / term / monthly_cost / upfront / savings; the
// synthetic execution row must too, or a single valid 1yr t4g.nano RI shows up
// as "0 Years / multiple / $0" because Term/MonthlyCost/ResourceType were never
// mapped.
//
//   - Single-rec execution (the common single-RI case in #631): project that
//     rec's fields verbatim — exactly what the completed row stores per
//     commitment. Costs come from the rec itself (UpfrontCost / MonthlyCost /
//     Savings), matching how purchase_history persists per-commitment dollars.
//   - Multi-rec execution: keep the aggregate display — "N commitment(s)" /
//     "multiple" region, term collapsed to the shared value (0 when recs
//     disagree), and execution-level cost totals. A single row cannot honestly
//     show one resource type or term for a heterogeneous basket.
func projectRecommendationFields(row *config.PurchaseHistoryRecord, exec config.PurchaseExecution) {
	recs := exec.Recommendations
	if len(recs) == 1 {
		r := recs[0]
		row.Service = r.Service
		row.ResourceType = r.ResourceType
		row.Region = r.Region
		row.Term = r.Term
		row.Payment = r.Payment
		row.UpfrontCost = &r.UpfrontCost
		row.EstimatedSavings = r.Savings
		row.MonthlyCost = r.MonthlyCost
		return
	}
	row.Region = "multiple"
	row.ResourceType = fmt.Sprintf("%d commitment(s)", len(recs))
	row.Service = collapseRecommendationService(recs)
	row.Term = collapseRecommendationTerm(recs)
	row.Payment = collapseRecommendationPayment(recs)
	row.MonthlyCost = sumRecommendationMonthlyCostPtr(recs)
	row.UpfrontCost = &exec.TotalUpfrontCost
	row.EstimatedSavings = exec.EstimatedSavings
}

// collapseRecommendationService returns the single service shared by every
// recommendation in a multi-rec execution, or "multiple" when they differ (or
// the slice is empty).
func collapseRecommendationService(recs []config.RecommendationRecord) string {
	if len(recs) == 0 {
		return "multiple"
	}
	s := recs[0].Service
	for _rvc := range recs[1:] {
		r := recs[1:][_rvc]
		if r.Service != s {
			return "multiple"
		}
	}
	return s
}

// collapseRecommendationTerm returns the term shared by every recommendation in
// a multi-rec execution, or 0 when they differ (or the slice is empty). 0
// renders as "" in the UI rather than a misleading single term for a basket
// that spans 1yr and 3yr commitments.
func collapseRecommendationTerm(recs []config.RecommendationRecord) int {
	if len(recs) == 0 {
		return 0
	}
	t := recs[0].Term
	for _rvc := range recs[1:] {
		r := recs[1:][_rvc]
		if r.Term != t {
			return 0
		}
	}
	return t
}

// collapseRecommendationProvider returns the single provider shared by every
// recommendation in an execution, or "multiple" when the execution spans
// providers (e.g. a plan that mixes AWS EC2 and Azure VM reservations).
func collapseRecommendationProvider(recs []config.RecommendationRecord) string {
	if len(recs) == 0 {
		return "multiple"
	}
	p := recs[0].Provider
	for _rvc := range recs[1:] {
		r := recs[1:][_rvc]
		if r.Provider != p {
			return "multiple"
		}
	}
	return p
}

// collapseRecommendationPayment returns the payment option shared by every
// recommendation in an execution, or "" when they disagree (or the slice is
// empty). Empty renders as the dash fallback in the Approval queue rather
// than a misleading single payment string for a basket that mixes options.
func collapseRecommendationPayment(recs []config.RecommendationRecord) string {
	if len(recs) == 0 {
		return ""
	}
	p := recs[0].Payment
	for _rvc := range recs[1:] {
		r := recs[1:][_rvc]
		if r.Payment != p {
			return ""
		}
	}
	return p
}

// collapseRecommendationAccount returns the cloud-account ID shared by every
// recommendation in an execution, or "" when they disagree (or none have one
// set). Used as the Account fallback when exec.CloudAccountID is nil —
// notably for web-initiated bulk purchases, which only populate the per-rec
// CloudAccountID and leave the execution-level field blank.
func collapseRecommendationAccount(recs []config.RecommendationRecord) string {
	if len(recs) == 0 {
		return ""
	}
	var first string
	if recs[0].CloudAccountID != nil {
		first = *recs[0].CloudAccountID
	}
	for _rvc := range recs[1:] {
		r := recs[1:][_rvc]
		var cur string
		if r.CloudAccountID != nil {
			cur = *r.CloudAccountID
		}
		if cur != first {
			return ""
		}
	}
	return first
}

// sumRecommendationMonthlyCostPtr sums the per-rec MonthlyCost values for a
// multi-rec execution row in the Approval queue. Nil entries (provider API
// did not return a monthly breakdown) are skipped. Returns nil when every rec
// has a nil MonthlyCost so the row renders as "—" rather than "$0.00".
// Returns a pointer to the accumulated total otherwise.
func sumRecommendationMonthlyCostPtr(recs []config.RecommendationRecord) *float64 {
	var total float64
	anyNonNil := false
	for _rvc := range recs {
		r := recs[_rvc]
		if r.MonthlyCost != nil {
			total += *r.MonthlyCost
			anyNonNil = true
		}
	}
	if !anyNonNil {
		return nil
	}
	return &total
}

// MaxHistoryDateRangeDays caps the inclusive start/end window the History
// handler accepts on a single request. Mirrors the analytics cap (issue
// #414 / PR #529): an unbounded range turns the WHERE-on-timestamp into a
// full-table scan over purchase_history, which the dashboard frontend
// neither needs nor renders coherently past a year. 366 admits a full leap
// year and rejects anything larger with 400.
const MaxHistoryDateRangeDays = 366

// historyFilters carries the shared filter set used by both halves of the
// merged /api/history response: the SQL path (purchase_history rows in
// fetchPurchaseHistory) and the in-memory path (synthesized execution rows
// in fetchExecutionsAsHistory). Keeping them in one struct guarantees the
// two halves stay scoped consistently — the bug behind issue #701 was that
// the executions path ignored the filters the SQL path was supposed to apply.
//
// Two account inputs are accepted but they are NOT interchangeable:
//   - LegacyAccountID is the singular `account_id` query param. It is the
//     cloud-provider account number (VARCHAR(20) — e.g. "123456789012"),
//     matched against purchase_history.account_id by GetPurchaseHistory.
//     Only the no-other-filters fast path consumes it; the filtered SQL
//     path ignores it because cloud_account_id is a UUID column.
//   - AccountIDs is the plural `account_ids` query param, a list of UUIDs
//     matched against purchase_history.cloud_account_id (the
//     cloud_accounts FK). This is what the frontend sends.
//
// HasDate is true iff start OR end was supplied. When false the Start/End
// times are zero-valued and the SQL/in-memory date predicates are skipped
// entirely (so legacy clients that don't send dates keep working).
type historyFilters struct {
	Start                 time.Time
	End                   time.Time
	ExternalIDsByProvider map[string][]string
	Provider              string
	LegacyAccountID       string
	// PlanID narrows the history to one purchase plan's purchases and
	// executions (the plan-history view's plan_id); empty means no plan filter.
	PlanID     string
	AccountIDs []string
	Limit      int
	HasDate    bool
	// Cursor is the stream position the page starts after (issue #674); nil
	// for the first page.
	Cursor *config.HistoryPageKey
}

// parseHistoryFilters validates and normalises the /api/history query string.
// Returns a 400 ClientError for malformed provider, non-UUID account_ids, a
// start/end that doesn't parse as YYYY-MM-DD, an inverted range
// (start > end), or a range exceeding MaxHistoryDateRangeDays.
func parseHistoryFilters(params map[string]string) (historyFilters, error) {
	var f historyFilters

	provider := params["provider"]
	if provider == "all" {
		provider = "" // "all" is the explicit "no filter" sentinel
	}
	if err := validateProvider(provider); err != nil {
		return f, err
	}
	f.Provider = provider

	accountIDs, err := parseAccountIDs(params["account_ids"])
	if err != nil {
		return f, NewClientError(400, err.Error())
	}
	f.AccountIDs = accountIDs
	f.LegacyAccountID = params["account_id"]

	planID, err := parseHistoryPlanID(params["plan_id"])
	if err != nil {
		return f, err
	}
	f.PlanID = planID

	start, end, hasDate, err := parseHistoryDateRange(params["start"], params["end"])
	if err != nil {
		return f, err
	}
	f.HasDate = hasDate
	f.Start = start
	f.End = end

	limit, err := parseHistoryLimit(params["limit"])
	if err != nil {
		return f, err
	}
	f.Limit = limit

	cursor, pagingErr := parseHistoryPaging(params)
	if pagingErr != nil {
		return f, pagingErr
	}
	f.Cursor = cursor

	return f, nil
}

// parseHistoryLimit parses the optional limit, clamped to [1, MaxListLimit]
// with DefaultListLimit for empty or non-positive values.
func parseHistoryLimit(raw string) (int, error) {
	limit := config.DefaultListLimit
	if raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return 0, NewClientError(400, "invalid limit: must be a positive integer")
		}
		limit = n
	}
	if limit < 1 {
		limit = config.DefaultListLimit
	}
	if limit > config.MaxListLimit {
		limit = config.MaxListLimit
	}
	return limit, nil
}

// parseHistoryPlanID validates the optional plan_id filter; empty means none.
// The value is trimmed and lowercased so the SQL predicate and the in-memory
// execution check compare the same canonical text.
func parseHistoryPlanID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", nil
	}
	if err := validateUUID(id); err != nil {
		return "", NewClientError(400, "invalid plan_id: must be a UUID")
	}
	return strings.ToLower(id), nil
}

// parseHistoryDateRange parses optional YYYY-MM-DD start/end strings. Both
// empty -> no date filter (hasDate=false, returned times are zero). One or
// both populated -> hasDate=true; an unset bound defaults to span the
// maximum allowed window from the supplied side. The window is capped at
// MaxHistoryDateRangeDays to mirror PR #529 (issue #414) and prevent a full-
// table-scan DoS via start=1970-01-01&end=2100-12-31.
//
// YYYY-MM-DD only (per issue #701 spec): the frontend's <input type="date">
// fields emit exactly that format, so accepting RFC 3339 here would be a
// surface area not exercised in production and a divergence from the
// analytics handler's broader format set.
func parseHistoryDateRange(startStr, endStr string) (time.Time, time.Time, bool, error) { //nolint:gocritic // unnamedResult: return names would conflict with body locals
	if startStr == "" && endStr == "" {
		return time.Time{}, time.Time{}, false, nil
	}
	start, end, err := parseHistoryDateBounds(startStr, endStr)
	if err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	const maxWindow = MaxHistoryDateRangeDays * 24 * time.Hour
	// Fill in the open side, if any, so the absent bound spans the maximum
	// allowed window from the supplied side. This keeps a "start only" or
	// "end only" call meaningful AND still bounded by the same DoS cap.
	switch {
	case startStr == "" && endStr != "":
		start = end.Add(-maxWindow)
	case startStr != "" && endStr == "":
		end = start.Add(maxWindow)
	}
	if start.After(end) {
		return time.Time{}, time.Time{}, false, NewClientError(400,
			"start date must be before or equal to end date")
	}
	if end.Sub(start) > maxWindow {
		return time.Time{}, time.Time{}, false, NewClientError(400,
			fmt.Sprintf("date range too large: maximum allowed range is %d days", MaxHistoryDateRangeDays))
	}
	return start, end, true, nil
}

// parseHistoryDateBounds parses each YYYY-MM-DD side independently. An empty
// string returns the zero value for that side; the caller is responsible for
// substituting a sensible default. The end side is rolled forward to end-of-
// day so a date input is inclusive of the chosen day.
func parseHistoryDateBounds(startStr, endStr string) (time.Time, time.Time, error) { //nolint:gocritic // unnamedResult: return names would conflict with body locals
	const layout = "2006-01-02"
	var start, end time.Time
	if startStr != "" {
		s, err := time.ParseInLocation(layout, startStr, time.UTC)
		if err != nil {
			return time.Time{}, time.Time{}, NewClientError(400,
				"invalid start date format: expected YYYY-MM-DD")
		}
		start = s
	}
	if endStr != "" {
		e, err := time.ParseInLocation(layout, endStr, time.UTC)
		if err != nil {
			return time.Time{}, time.Time{}, NewClientError(400,
				"invalid end date format: expected YYYY-MM-DD")
		}
		// Make end inclusive of the requested day (frontend semantics: a
		// user picking 2024-01-31 expects rows from that day to render).
		end = e.Add(24*time.Hour - time.Second)
	}
	return start, end, nil
}

// matchesExecution reports whether a PurchaseExecution should be retained
// after applying the request filters in Go (the SQL path is on
// purchase_history; this in-memory equivalent keeps the two halves of the
// merged response consistently scoped, issue #701).
//
//   - Provider: matches when ANY recommendation in the execution carries
//     the filter value. A single-rec execution collapses to one provider; a
//     multi-rec basket that spans providers (e.g. aws+azure) matches any of
//     them — dropping such an execution because its collapsed display label
//     is "multiple" would hide real activity the user owns.
//   - Account: matches when the execution's effective account ID is one of the
//     filtered IDs. The effective account ID is exec.CloudAccountID when set;
//     otherwise collapseRecommendationAccount(exec.Recommendations) provides
//     the fallback, matching the same logic used in executionToHistoryRow.
//     Web-initiated bulk purchases never set exec.CloudAccountID — only the
//     per-rec field is populated — so without this fallback an account-filtered
//     approval queue would silently drop all pending web purchases (issue #704).
//     An execution whose effective account ID is "" (nil exec field AND recs
//     disagree or have no account) is excluded once account_ids is non-empty.
//   - Date: matches when ScheduledDate is within [Start, End]. Inclusive
//     both sides; End is the end-of-day for YYYY-MM-DD inputs.
func (f historyFilters) matchesExecution(exec config.PurchaseExecution) bool {
	if !f.matchesExecutionProviderAndAccount(exec) {
		return false
	}
	if f.PlanID != "" && exec.PlanID != f.PlanID {
		return false
	}
	if f.HasDate {
		if exec.ScheduledDate.Before(f.Start) || exec.ScheduledDate.After(f.End) {
			return false
		}
	}
	return true
}

// matchesExecutionProviderAndAccount is matchesExecution without the date
// check. The History summary aggregate pushes the date bound into SQL
// (scheduled_date between Start and End, inclusive) and applies this to each
// execution group, so the row list and the totals share one provider and
// account predicate.
func (f historyFilters) matchesExecutionProviderAndAccount(exec config.PurchaseExecution) bool {
	if f.Provider != "" {
		if !executionHasProvider(exec, f.Provider) {
			return false
		}
	}
	if len(f.AccountIDs) > 0 || len(f.ExternalIDsByProvider) > 0 {
		if !accountMatchesFilters(exec, f.AccountIDs, f.ExternalIDsByProvider) {
			return false
		}
	}
	return true
}

// accountMatchesFilters pulls the dual-id account-match logic out of
// matchesExecution to keep it under the cyclomatic limit.
//
// AccountIDs are cloud_accounts UUIDs; externalIDsByProvider are the
// cloud-provider external account numbers resolved from them, grouped by
// provider. An execution's effective account ID may be EITHER representation:
// exec.CloudAccountID and the per-rec CloudAccountID hold the UUID for
// UUID-attributed executions, while a legacy/ambient execution may only carry
// the external number. Match against both so an external-id-only pending row is
// not dropped (the mirror of the SQL dual-column predicate, issue #701/#498).
//
// The external-id match is provider-scoped: an external number matches only when
// it is listed under the execution's own provider, or under the "" key (unknown
// provider, legacy behavior). This mirrors the SQL (provider = $p AND
// account_id = ANY(...)) and keeps a reused external number across providers
// (aws/123 vs azure/123) from matching the wrong execution.
//
// Uses the same two-level account resolution as executionToHistoryRow:
// exec.CloudAccountID first, then the rec-level fallback, so web bulk-purchase
// executions (exec.CloudAccountID == nil) are not silently dropped.
func accountMatchesFilters(exec config.PurchaseExecution, accountIDs []string, externalIDsByProvider map[string][]string) bool {
	var accountID string
	if exec.CloudAccountID != nil {
		accountID = *exec.CloudAccountID
	} else {
		accountID = collapseRecommendationAccount(exec.Recommendations)
	}
	if accountID == "" {
		return false
	}
	if stringInSlice(accountID, accountIDs) {
		return true
	}
	// External-id half: only the execution's own provider group (plus the ""
	// unknown-provider group) may match, so a reused external number across
	// providers can't leak.
	provider := executionProvider(exec)
	if provider != "" && stringInSlice(accountID, externalIDsByProvider[provider]) {
		return true
	}
	return stringInSlice(accountID, externalIDsByProvider[""])
}

// executionProvider returns the execution's provider, taken from its first
// recommendation (all recs in an execution share a provider in practice).
// Returns "" when no recommendation carries one.
func executionProvider(exec config.PurchaseExecution) string {
	for _rvc := range exec.Recommendations {
		r := exec.Recommendations[_rvc]
		if r.Provider != "" {
			return r.Provider
		}
	}
	return ""
}

// executionHasProvider reports whether any of the execution's
// recommendations carries the given provider value.
func executionHasProvider(exec config.PurchaseExecution, provider string) bool {
	for _rvc := range exec.Recommendations {
		r := exec.Recommendations[_rvc]
		if r.Provider == provider {
			return true
		}
	}
	return false
}

// stringInSlice is a tiny linear-search helper for the per-execution
// account filter. The slice is bounded by MaxAccountIDsPerRequest (200) so
// a linear scan is comfortably cheaper than the map allocation a set would
// require, and the call is hot only for executions (DB rows go through SQL).
func stringInSlice(needle string, haystack []string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// fetchPurchaseHistory reads purchases from the store, applying the parsed
// filter set. The store-level filtered method (issue #701) pushes provider /
// account / timestamp range into SQL. The account predicate is dual-column
// (cloud_account_id = ANY(AccountIDs) OR (provider = $p AND account_id =
// ANY(ExternalIDsByProvider[p]))) so a row that carries only one of the two
// account representations is still matched — the original #701/#498/#866 bug was
// that filtering on the sparse cloud_account_id UUID column alone dropped every
// direct-execute/ambient row. Grouping the external ids by provider keeps a
// reused external number across providers from leaking the wrong rows.
//
// It reads one keyset batch of the History stream: up to limit rows after
// `after` (nil starts at the newest row), in stream order (issue #674). An
// empty filter set selects every row. The legacy singular account_id
// (LegacyAccountID) is resolved to the same dual-column inputs by the caller
// (getHistory) and arrives here folded into AccountIDs/ExternalIDsByProvider,
// so there is no longer a separate single-column legacy path to coerce
// identifiers through.
func (h *Handler) fetchPurchaseHistory(ctx context.Context, filters *historyFilters, after *config.HistoryPageKey, limit int) ([]config.PurchaseHistoryPageRow, error) {
	filter := filters.purchaseHistoryFilter()
	filter.Limit = limit
	return h.config.ListPurchaseHistoryPage(ctx, filter, after)
}

// purchaseHistoryFilter maps the parsed filters onto the store filter. Both the
// History row fetch and the summary aggregate use it, so they hand the store
// the same predicate inputs. An empty historyFilters maps to an empty store
// filter, which selects every row.
func (f historyFilters) purchaseHistoryFilter() config.PurchaseHistoryFilter {
	out := config.PurchaseHistoryFilter{
		Provider:              f.Provider,
		PlanID:                f.PlanID,
		AccountIDs:            f.AccountIDs,
		ExternalIDsByProvider: f.ExternalIDsByProvider,
		Limit:                 f.Limit,
	}
	if f.HasDate {
		s, e := f.Start, f.End
		out.Start = &s
		out.End = &e
	}
	return out
}

// resolveHistoryAccountFilter populates filters.AccountIDs /
// filters.ExternalIDsByProvider from the parsed account params so the SQL and
// in-memory paths share one dual-column account set. The plural `account_ids`
// (UUIDs) are resolved to their per-provider external numbers via
// resolveAccountFilterIDs. The singular legacy `account_id` is folded in too: it
// is a top-bar chip UUID for current callers (resolved to UUID + external) or a
// raw external number for pre-UUID callers (grouped under the "" provider key).
// Best-effort: resolution failures leave the UUID-only set in place (no worse
// than the pre-fix behavior), and the per-record allowed_accounts filter still
// enforces scoping downstream.
func (h *Handler) resolveHistoryAccountFilter(ctx context.Context, filters *historyFilters) {
	uuids, externalsByProvider := h.resolveAccountFilterIDs(ctx, filters.AccountIDs)

	if filters.LegacyAccountID != "" {
		lUUIDs, lExternalsByProvider := h.resolveSingleAccountFilterIDs(ctx, filters.LegacyAccountID)
		uuids = appendMissing(uuids, lUUIDs...)
		for provider, exts := range lExternalsByProvider {
			for _, ext := range exts {
				externalsByProvider = addExternalIDForProvider(externalsByProvider, provider, ext)
			}
		}
	}

	filters.AccountIDs = uuids
	filters.ExternalIDsByProvider = externalsByProvider
}

// appendMissing appends each value to dst only if not already present,
// preserving order. Used to merge the legacy single-account ids into the
// plural sets without introducing duplicate ANY() bind values.
func appendMissing(dst []string, vals ...string) []string {
	for _, v := range vals {
		if v == "" || stringInSlice(v, dst) {
			continue
		}
		dst = append(dst, v)
	}
	return dst
}

// filterPurchaseHistoryByAllowedAccounts drops records whose AccountID/Name
// is outside the session's allowed_accounts. Admin/unrestricted sessions pass
// through unchanged.
//
// Rows with an empty AccountID are exempt from the drop for scoped users
// only when the requesting user created the row (issue #1032, regression of
// #621). An empty AccountID means the execution was ambient
// (exec.CloudAccountID == nil) AND its recommendations carry no common
// account. These are in-flight financial actions the user owns that cannot be
// attributed to a specific cloud account; dropping them silently re-introduces
// the #621 disappearance bug. The exemption is now gated on ownership so that
// another user's multi-account in-flight row (including CreatedByUserEmail PII
// and dollar amounts) is not visible to unrelated scoped users.
func (h *Handler) filterPurchaseHistoryByAllowedAccounts(ctx context.Context, session *Session, purchases []config.PurchaseHistoryRecord) ([]config.PurchaseHistoryRecord, error) {
	scope, err := h.loadHistoryScope(ctx, session)
	if err != nil {
		return nil, err
	}
	return scope.filter(purchases), nil
}

// historyRowAccountName returns the registered account name for a history row,
// or "" when it cannot be determined unambiguously. Without CloudAccountID it
// tries the account UUID first (in-flight rows store the UUID in AccountID),
// then provider plus external id, since external ids are unique only per
// provider. A legacy row without a provider matches only when exactly one
// account across all providers has that external id.
func historyRowAccountName(accounts []config.CloudAccount, p config.PurchaseHistoryRecord) string {
	if p.CloudAccountID != nil {
		return accountNameByID(accounts, *p.CloudAccountID)
	}
	if name := accountNameByID(accounts, p.AccountID); name != "" {
		return name
	}
	if p.Provider != "" {
		return accountNameByExternalID(accounts, p.Provider, p.AccountID)
	}
	var name string
	matches := 0
	for i := range accounts {
		if accounts[i].ExternalID == p.AccountID {
			name = accounts[i].Name
			matches++
		}
	}
	if matches != 1 {
		return ""
	}
	return name
}

func summarizePurchaseHistory(purchases []config.PurchaseHistoryRecord) HistorySummary {
	summary := HistorySummary{TotalUpfront: new(float64)}
	for i := range purchases {
		addHistoryRowToSummary(&summary, &purchases[i], 1)
	}
	summary.TotalAnnualSavings = summary.TotalMonthlySavings * 12
	return summary
}

// addHistoryRowToSummary adds n rows shaped like p to the summary. The row
// list (n == 1 per row) and the SQL aggregate (one call per group, with p
// carrying the group's dollar sums) both go through it, so the totals use one
// set of bucketing rules.
func addHistoryRowToSummary(summary *HistorySummary, p *config.PurchaseHistoryRecord, n int) {
	summary.TotalPurchases += n
	// Revoked commitments are excluded from dollar KPIs regardless of their
	// status: the provider has cancelled the commitment, so UpfrontCost and
	// EstimatedSavings no longer represent active spend or realized savings.
	// They count toward TotalPurchases (for auditability) and TotalRevoked
	// (so the UI can surface the count), but not TotalCompleted.
	// GetActivePurchaseHistory already excludes revoked rows via SQL; this
	// guard handles the GetPurchaseHistoryFiltered path used by History.
	if p.RevokedAt != nil {
		summary.TotalRevoked += n
		return
	}
	if addNonCompletedRowToSummary(summary, p.Status, n) {
		return
	}
	summary.TotalCompleted += n
	// Audit-gap completed rows (issue #621) are synthesized execution rows
	// whose purchase_history write failed. Count them as completed (the
	// money WAS committed and they must stay visible) but exclude their
	// execution-level dollars: a partially-saved multi-rec execution can
	// have BOTH some purchase_history rows AND this synthesized row, and
	// adding the full execution total here would double-count the recs that
	// did save. The dollars are surfaced via the individual purchase_history
	// rows that succeeded; the synthesized row is the audit flag, not a
	// money source. IsAuditGap is the explicit marker: real purchase_history
	// rows loaded from the DB always leave it false, so a future change that
	// annotates completed DB rows can't silently drop them from the totals.
	if p.IsAuditGap {
		return
	}
	summary.TotalUpfront = sumKnownCosts(summary.TotalUpfront, p.UpfrontCost)
	summary.TotalMonthlySavings += p.EstimatedSavings
}

// addNonCompletedRowToSummary counts n rows in their non-completed bucket and
// reports whether status was one. Non-completed rows count toward
// TotalPurchases and their specific bucket (pending / in-progress / failed /
// expired / canceled) but are excluded from the dollar totals: the money
// hasn't been committed for any of those states. "completed" and unset
// (legacy DB rows that pre-date the status field) both count as completed.
func addNonCompletedRowToSummary(summary *HistorySummary, status string, n int) bool {
	switch status {
	case "pending", "notified", "scheduled":
		// "scheduled" (issue #239) is a pre-purchase state like
		// pending/notified (the cloud SDK has not been called yet;
		// scheduled is revoked, not canceled). It must not
		// fall through to TotalCompleted, or the
		// "Total Upfront Spent" card counts money not yet spent.
		summary.TotalPending += n
	case "approved", "running", "paused":
		// In-flight (issue #621). Not yet confirmed final — keep out of the
		// committed dollar totals so an interrupted approval can't inflate
		// reported spend/savings.
		summary.TotalInProgress += n
	case "failed":
		summary.TotalFailed += n
	case "expired":
		summary.TotalExpired += n
	case config.StatusCanceled, config.LegacyStatusCanceled:
		// A canceled purchase represents zero committed spend and zero
		// realized savings (issue #736). Exclude from all dollar KPIs and
		// from TotalCompleted -- the money was never committed. The legacy
		// British spelling is matched alongside the US one during the
		// expand-contract rename (migration 000089) so legacy rows can't
		// inflate KPIs mid-deploy; the contract migration (#1278) drops it
		// once the deploy is stable.
	default:
		return false
	}
	return true
}
