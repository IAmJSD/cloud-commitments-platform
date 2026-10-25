package api

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// purchaseHistoryRowStatus is the Status the History view gives every
// purchase_history row, both on the listed rows and on the aggregate groups the
// summary is built from.
const purchaseHistoryRowStatus = "completed"

// historyExecutionStatuses is every execution status the History view lists:
// the actionable class followed by the terminal class.
func historyExecutionStatuses() []string {
	return slices.Concat(actionableExecutionStatuses, terminalExecutionStatuses)
}

// historyScope is the account-scope check applied to History rows. It is
// resolved once per request and applied both to the listed rows and to the
// groups the summary totals are built from, so a scoped user's totals never
// include a row the scoped user could not see in the list (issue #675).
type historyScope struct {
	allowed  auth.AccountScope
	accounts []config.CloudAccount
	userID   string
}

// loadHistoryScope resolves the session's account scope. The cloud account
// list is loaded only for a restricted scope, because only allows needs it.
func (h *Handler) loadHistoryScope(ctx context.Context, session *Session) (historyScope, error) {
	allowed, err := h.getAccountScope(ctx, session)
	if err != nil {
		return historyScope{}, fmt.Errorf("failed to get allowed accounts: %w", err)
	}
	scope := historyScope{allowed: allowed, userID: session.UserID}
	if allowed.AllowsAll() {
		return scope, nil
	}
	scope.accounts, err = h.config.ListCloudAccounts(ctx, config.CloudAccountFilter{})
	if err != nil {
		return historyScope{}, fmt.Errorf("failed to list cloud accounts: %w", err)
	}
	return scope, nil
}

// allows reports whether the row is inside the scope.
//
// Rows with an empty AccountID are exempt from the drop for scoped users only
// when the requesting user created the row (issue #1032, regression of #621).
// An empty AccountID means the execution was ambient (exec.CloudAccountID ==
// nil) AND its recommendations carry no common account. These are in-flight
// financial actions the user owns that cannot be attributed to a specific
// cloud account; dropping them silently re-introduces the #621 disappearance
// bug. The exemption is gated on ownership so that another user's
// multi-account in-flight row (including CreatedByUserEmail PII and dollar
// amounts) is not visible to unrelated scoped users.
func (s historyScope) allows(p *config.PurchaseHistoryRecord) bool {
	if s.allowed.AllowsAll() {
		return true
	}
	if p.AccountID == "" {
		return p.CreatedByUserID == s.userID
	}
	return s.allowed.Allows(p.AccountID, historyRowAccountName(s.accounts, *p))
}

// filter returns the rows inside the scope. Admin/unrestricted scopes pass the
// slice through unchanged.
func (s historyScope) filter(purchases []config.PurchaseHistoryRecord) []config.PurchaseHistoryRecord {
	if s.allowed.AllowsAll() {
		return purchases
	}
	filtered := make([]config.PurchaseHistoryRecord, 0, len(purchases))
	for i := range purchases {
		if s.allows(&purchases[i]) {
			filtered = append(filtered, purchases[i])
		}
	}
	return filtered
}

// historySummary returns the summary over every row matching the filters and
// the scope, and true. When the aggregate query fails it logs, falls back to
// summarizing the listed rows (which under-reports once a cap is hit) and
// returns false, so the response can say the totals cover only the page.
func (h *Handler) historySummary(ctx context.Context, filters historyFilters, scope historyScope, listed []config.PurchaseHistoryRecord) (HistorySummary, bool) {
	summary, err := h.aggregateHistorySummary(ctx, filters, scope)
	if err != nil {
		logging.Warnf("history: summary aggregate failed, summarizing the listed rows only: %v", err)
		return summarizePurchaseHistory(listed), false
	}
	return summary, true
}

// aggregateHistorySummary builds the History summary from the store's grouped
// counts and sums. The store applies the same SQL predicates as the row
// fetches (the purchase_history filter, the execution status predicate and the
// date bound); this function applies the remaining checks the row path runs in
// Go, with the same functions: the execution provider/account filter, the row
// projection (executionToHistoryRow) and the account scope.
func (h *Handler) aggregateHistorySummary(ctx context.Context, filters historyFilters, scope historyScope) (HistorySummary, error) {
	groups, err := h.config.HistorySummaryAggregate(ctx, historySummaryFilter(filters))
	if err != nil {
		return HistorySummary{}, err
	}
	summary := HistorySummary{TotalUpfront: new(float64)}
	for i := range groups.Purchases {
		row := purchaseGroupRow(&groups.Purchases[i])
		if scope.allows(&row) {
			addHistoryRowToSummary(&summary, &row, groups.Purchases[i].Count)
		}
	}
	for i := range groups.Executions {
		g := &groups.Executions[i]
		exec := config.PurchaseExecution{
			Status:          g.Status,
			CloudAccountID:  g.CloudAccountID,
			CreatedByUserID: g.CreatedByUserID,
			Recommendations: g.Recommendations,
		}
		if !filters.matchesExecutionProviderAndAccount(exec) {
			continue
		}
		row := executionToHistoryRow(exec, "", "")
		if scope.allows(&row) {
			addHistoryRowToSummary(&summary, &row, g.Count)
		}
	}
	summary.TotalAnnualSavings = summary.TotalMonthlySavings * 12
	return summary, nil
}

// historySummaryFilter maps the parsed filters onto the aggregate's filter:
// the purchase_history half gets the row fetch's filter, the executions half
// gets every listed status and the date bound the row path checks in Go.
func historySummaryFilter(filters historyFilters) config.HistorySummaryFilter {
	out := config.HistorySummaryFilter{
		History:           filters.purchaseHistoryFilter(),
		ExecutionStatuses: historyExecutionStatuses(),
	}
	if filters.HasDate {
		out.ExecutionStart = out.History.Start
		out.ExecutionEnd = out.History.End
	}
	return out
}

// revokedGroupMarker stands in for revoked_at on a revoked aggregate group;
// only its non-nil-ness is read.
var revokedGroupMarker = time.Unix(0, 0).UTC()

// purchaseGroupRow shapes a purchase_history group like one listed row, with
// the group's dollar sums in place of a single row's.
func purchaseGroupRow(g *config.PurchaseHistorySummaryGroup) config.PurchaseHistoryRecord {
	row := config.PurchaseHistoryRecord{
		AccountID:        g.AccountID,
		CloudAccountID:   g.CloudAccountID,
		Provider:         g.Provider,
		Status:           purchaseHistoryRowStatus,
		UpfrontCost:      g.UpfrontCost,
		EstimatedSavings: g.EstimatedSavings,
	}
	if g.Revoked {
		t := revokedGroupMarker
		row.RevokedAt = &t
	}
	return row
}
