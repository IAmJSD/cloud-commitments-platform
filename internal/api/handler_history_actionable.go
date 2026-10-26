package api

import (
	"context"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// actionableExecutionsQuery is the SQL read for the History view's actionable
// class (issue #632). It carries the request filters and the caller's account
// scope so the store applies them before the MaxListLimit cap. Each predicate
// is a superset of the Go check the handler still runs (matchesExecution and
// historyScope.allows), so pushing it down can only drop rows Go would drop.
func actionableExecutionsQuery(filters *historyFilters, scope *historyScope) config.ExecutionsByStatusesQuery {
	q := config.ExecutionsByStatusesQuery{
		Statuses: actionableExecutionStatuses,
		Limit:    config.MaxListLimit,
		Provider: filters.Provider,
		Scope:    scope.executionScope(),
	}
	if filters.HasDate {
		start, end := filters.Start, filters.End
		q.Start, q.End = &start, &end
	}
	if len(filters.AccountIDs) > 0 || len(filters.ExternalIDsByProvider) > 0 {
		q.AccountIDs = executionFilterAccountValues(filters)
	}
	return q
}

// executionFilterAccountValues lists every account value accountMatchesFilters
// can match: the UUIDs and the external numbers of every provider. The
// provider pairing of an external number is left to the Go check, so this is
// a superset of it.
func executionFilterAccountValues(filters *historyFilters) []string {
	values := append([]string{}, filters.AccountIDs...)
	for _, ids := range filters.ExternalIDsByProvider {
		values = appendMissing(values, ids...)
	}
	return values
}

// executionScope returns the SQL form of the scope, or nil for an
// unrestricted caller. It lists every account value allows can accept: the
// scope's literal entries (allows compares the row's account against them
// directly) plus the UUID and external number of each registered account the
// scope allows, since allows also matches a row by the name of the account
// its UUID or external number resolves to. An empty list keeps only the
// caller's own unattributed rows, as allows does.
func (s *historyScope) executionScope() *config.ExecutionScope {
	if s.allowed.AllowsAll() {
		return nil
	}
	values := appendMissing(nil, s.allowed.Accounts...)
	for i := range s.accounts {
		a := &s.accounts[i]
		if s.allowed.Allows(a.ID, a.Name) {
			values = appendMissing(values, a.ID, a.ExternalID)
		}
	}
	return &config.ExecutionScope{AccountIDs: values, UserID: s.userID}
}

// isNarrowed reports whether q drops any row the unfiltered class read would
// return.
func isNarrowed(q *config.ExecutionsByStatusesQuery) bool {
	return q.Start != nil || q.End != nil || q.Provider != "" || len(q.AccountIDs) > 0 || q.Scope != nil
}

// staleExecutionsToSweep returns the stale pending/notified executions the GET
// expires after it responds (see expireStaleExecutions). The sweep is not
// limited to the caller's filters or scope: it never shows a row, and the
// History load is what expires stale approvals and releases their
// suppressions. So when q is narrowed, the stale rows are read with a
// separate unfiltered query; otherwise they are taken from the class already
// read. A read failure is logged and skips the sweep for this request.
func (h *Handler) staleExecutionsToSweep(ctx context.Context, q *config.ExecutionsByStatusesQuery, executions []config.PurchaseExecution) []config.PurchaseExecution {
	if isNarrowed(q) {
		cutoff := time.Now().Add(-approvalExpiryWindow)
		rows, err := h.config.GetExecutionsByStatusesFiltered(ctx, config.ExecutionsByStatusesQuery{
			Statuses: []string{"pending", "notified"},
			Limit:    config.DefaultListLimit,
			End:      &cutoff,
		})
		if err != nil {
			logging.Warnf("history: failed to load stale executions to expire: %v", err)
			return nil
		}
		executions = rows
	}
	var stale []config.PurchaseExecution
	for i := range executions {
		if isStaleExecution(executions[i]) {
			stale = append(stale, executions[i])
		}
	}
	return stale
}
