package api

import (
	"context"
	"strings"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// upcomingFilter is the optional provider and account narrowing for
// GET /api/dashboard/upcoming (issue #713), so the Home widget can follow the
// same topbar chips as /api/dashboard/summary. The zero value keeps every row
// the session scope already allows.
type upcomingFilter struct {
	// accounts is nil when the caller sent no account filter. Otherwise it
	// holds the requested accounts already intersected with the session's
	// allowed_accounts by resolveDashboardAccountScope, so an out-of-scope
	// account is dropped (an empty set matches nothing) instead of widening
	// the result.
	accounts *dualColumnAccountSet
	// provider is "" for every provider.
	provider string
}

// dualColumnAccountSet is a resolved account filter: cloud_accounts UUIDs plus
// external account numbers grouped by provider ("" = provider unknown, from a
// legacy account_id that is not a known UUID).
type dualColumnAccountSet struct {
	externalIDsByProvider map[string][]string
	uuids                 []string
}

// parseUpcomingFilter reads the provider, account_ids and legacy account_id
// query parameters with the same validation and precedence as
// getDashboardSummary: an unknown provider or a malformed account_ids is a 400,
// and account_ids wins over account_id.
func (h *Handler) parseUpcomingFilter(ctx context.Context, session *Session, params map[string]string) (upcomingFilter, error) {
	provider, err := providerFilterParam(params["provider"])
	if err != nil {
		return upcomingFilter{}, err
	}
	f := upcomingFilter{provider: provider}

	requested, err := parseAccountIDs(params["account_ids"])
	if err != nil {
		return upcomingFilter{}, NewClientError(400, err.Error())
	}
	if len(requested) == 0 && params["account_id"] == "" {
		// No account chip: leave the rows to the session scope alone.
		return f, nil
	}
	uuids, externalIDsByProvider, err := h.resolveDashboardAccountScope(ctx, session, params)
	if err != nil {
		return upcomingFilter{}, err
	}
	f.accounts = &dualColumnAccountSet{uuids: uuids, externalIDsByProvider: externalIDsByProvider}
	return f, nil
}

// matches reports whether an upcoming row passes the filter. The provider is
// compared with the row's provider (its plan's first service, as shown in the
// widget). An execution matches the account filter when any account it can buy
// in is selected: a plan-level parent fans out to every plan account, so it is
// relevant to each of them, while a fan-out child carries its single account.
func (f upcomingFilter) matches(ctx context.Context, h *Handler, row *UpcomingPurchase, exec *config.PurchaseExecution, planAccounts map[string][]config.CloudAccount) (bool, error) {
	if f.provider != "" && !strings.EqualFold(row.Provider, f.provider) {
		return false, nil
	}
	if f.accounts == nil {
		return true, nil
	}
	accounts, err := h.executionAccounts(ctx, exec, planAccounts)
	if IsNotFoundError(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return f.accounts.matchesAny(accounts), nil
}

// matchesAny reports whether any account is in the set, by UUID or by external
// account number under its provider (or under "" for an unknown provider).
func (s *dualColumnAccountSet) matchesAny(accounts []config.CloudAccount) bool {
	for i := range accounts {
		a := &accounts[i]
		if stringInSlice(a.ID, s.uuids) {
			return true
		}
		if a.ExternalID == "" {
			continue
		}
		if stringInSlice(a.ExternalID, s.externalIDsByProvider[a.Provider]) ||
			stringInSlice(a.ExternalID, s.externalIDsByProvider[""]) {
			return true
		}
	}
	return false
}
