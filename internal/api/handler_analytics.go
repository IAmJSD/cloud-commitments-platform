// Package api provides the HTTP API handlers for analytics endpoints.
package api

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/analytics"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
)

// TrendsResponse is the savings-snapshot time-series for the Trends view: a
// monthly series (coverage %, committed spend, usage, realized savings) plus
// by-provider and by-service breakdowns over the requested window. Backed by
// the savings_snapshots store / materialized views (issues #1023 / #1033),
// distinct from the purchase_history-backed /history/analytics path.
type TrendsResponse struct {
	Start    string                        `json:"start"`
	End      string                        `json:"end"`
	Monthly  []analytics.MonthlySummary    `json:"monthly"`
	Provider []analytics.ProviderBreakdown `json:"by_provider"`
	Service  []analytics.ServiceBreakdown  `json:"by_service"`
	Months   int                           `json:"months"`
}

// getAnalyticsTrends handles GET /api/analytics/trends. It returns the
// historical savings-snapshot series scoped to the caller's allowed_accounts.
func (h *Handler) getAnalyticsTrends(ctx context.Context, req *events.LambdaFunctionURLRequest, params map[string]string) (any, error) {
	session, err := h.requirePermission(ctx, req, "view", "purchases")
	if err != nil {
		return nil, err
	}

	if h.analyticsSnapshots == nil {
		// Mirror getHistoryAnalytics: 503 = feature intentionally unavailable.
		return nil, NewClientError(503, "analytics snapshots not configured")
	}

	// Enforce allowed_accounts scope and resolve the requested account (top-bar
	// chip UUID, raw external id, or "" for all-accessible for an unrestricted
	// session) into the dual-column filter inputs so rows carrying only the
	// external account_id (cloud_account_id NULL) are matched. A scoped user can
	// never widen to "all" (empty filters mean all-accessible).
	requested, err := requestedAnalyticsAccounts(params)
	if err != nil {
		return nil, err
	}
	accountUUIDs, accountExternalIDsByProvider, err := h.resolveAnalyticsAccountFilter(ctx, session, requested)
	if err != nil {
		return nil, err
	}

	start, end, err := parseDateRange(params["start"], params["end"])
	if err != nil {
		return nil, err
	}

	months := monthsBetween(start, end)

	monthly, err := h.analyticsSnapshots.QueryMonthlyTotals(ctx, accountUUIDs, accountExternalIDsByProvider, months)
	if err != nil {
		return nil, fmt.Errorf("failed to query monthly totals: %w", err)
	}
	byProvider, err := h.analyticsSnapshots.QueryByProvider(ctx, accountUUIDs, accountExternalIDsByProvider, start, end)
	if err != nil {
		return nil, fmt.Errorf("failed to query provider breakdown: %w", err)
	}
	provider := params["provider"]
	byService, err := h.analyticsSnapshots.QueryByService(ctx, accountUUIDs, accountExternalIDsByProvider, provider, start, end)
	if err != nil {
		return nil, fmt.Errorf("failed to query service breakdown: %w", err)
	}

	return &TrendsResponse{
		Start:    start.Format(time.RFC3339),
		End:      end.Format(time.RFC3339),
		Months:   months,
		Monthly:  monthly,
		Provider: byProvider,
		Service:  byService,
	}, nil
}

// monthsBetween returns the inclusive count of month-buckets spanned by
// [start, end], at least 1. Used to bound the monthly_savings_summary query.
func monthsBetween(start, end time.Time) int {
	months := (end.Year()-start.Year())*12 + int(end.Month()) - int(start.Month()) + 1
	if months < 1 {
		return 1
	}
	return months
}

// AnalyticsResponse represents the response for the analytics endpoint.
type AnalyticsResponse struct {
	Start      string             `json:"start"`
	End        string             `json:"end"`
	Interval   string             `json:"interval"`
	Summary    *HistorySummary    `json:"summary"`
	DataPoints []HistoryDataPoint `json:"data_points"`
}

// BreakdownResponse represents the response for the breakdown endpoint.
type BreakdownResponse struct {
	Data      map[string]BreakdownValue `json:"data"`
	Dimension string                    `json:"dimension"`
	Start     string                    `json:"start"`
	End       string                    `json:"end"`
}

// getHistoryAnalytics handles GET /history/analytics.
func (h *Handler) getHistoryAnalytics(ctx context.Context, req *events.LambdaFunctionURLRequest, params map[string]string) (any, error) {
	// Analytics aggregate across purchase history — gate on view:purchases and
	// scope by allowed_accounts.
	session, err := h.requirePermission(ctx, req, "view", "purchases")
	if err != nil {
		return nil, err
	}

	// Analytics is Postgres-backed (api.PostgresAnalyticsClient) and wired
	// in server.Application.reinitializeAfterConnect, so analyticsClient
	// is non-nil whenever the DB is up. The guard stays as defense-in-depth
	// for test builds and misconfigured callers — the frontend treats 503
	// as "feature intentionally unavailable" and renders the corresponding
	// empty-state instead of a generic error.
	if h.analyticsClient == nil {
		return nil, NewClientError(503, "analytics not configured")
	}

	// Parse parameters
	interval := params["interval"]
	if interval == "" {
		interval = "hourly"
	}

	// Normalise + validate the global-filter provider chip, mirroring
	// parseHistoryFilters: "all" (and "") mean no provider filter, anything
	// else must be a known provider. Without this the chart ignored the
	// provider chip entirely (issue #498, QA 2.3).
	provider := params["provider"]
	if provider == "all" {
		provider = "" // explicit "no filter" sentinel
	}
	err = validateProvider(provider)
	if err != nil {
		return nil, err
	}

	// For scoped users we require account_id and validate it's in their scope.
	// We don't (yet) support analytics across a subset — the underlying
	// aggregate takes a single account_id. An unrestricted/admin session
	// can pass "" to mean "all accessible accounts". The requested account (a
	// top-bar chip UUID, a raw external id, or "" for all) resolves to the
	// dual-column filter inputs so rows that carry only the external account_id
	// (cloud_account_id NULL) are aggregated (issue #701/#498/#866).
	requested, err := requestedAnalyticsAccounts(params)
	if err != nil {
		return nil, err
	}
	accountUUIDs, accountExternalIDsByProvider, err := h.resolveAnalyticsAccountFilter(ctx, session, requested)
	if err != nil {
		return nil, err
	}

	// Parse date range
	start, end, err := parseDateRange(params["start"], params["end"])
	if err != nil {
		return nil, err
	}

	// Aggregate history from the analytics client (Postgres-backed).
	dataPoints, summary, err := h.analyticsClient.QueryHistory(ctx, accountUUIDs, accountExternalIDsByProvider, provider, start, end, interval)
	if err != nil {
		return nil, fmt.Errorf("failed to query analytics: %w", err)
	}

	return &AnalyticsResponse{
		Start:      start.Format(time.RFC3339),
		End:        end.Format(time.RFC3339),
		Interval:   interval,
		Summary:    summary,
		DataPoints: dataPoints,
	}, nil
}

// getHistoryBreakdown handles GET /history/breakdown.
func (h *Handler) getHistoryBreakdown(ctx context.Context, req *events.LambdaFunctionURLRequest, params map[string]string) (any, error) {
	session, err := h.requirePermission(ctx, req, "view", "purchases")
	if err != nil {
		return nil, err
	}

	if h.analyticsClient == nil {
		// See getHistoryAnalytics for the 503 rationale.
		return nil, NewClientError(503, "analytics not configured")
	}

	dimension := params["dimension"]
	if dimension == "" {
		dimension = "service"
	}

	// Scope check plus account -> dual-column inputs; see getHistoryAnalytics.
	requested, err := requestedAnalyticsAccounts(params)
	if err != nil {
		return nil, err
	}
	accountUUIDs, accountExternalIDsByProvider, err := h.resolveAnalyticsAccountFilter(ctx, session, requested)
	if err != nil {
		return nil, err
	}

	start, end, err := parseDateRange(params["start"], params["end"])
	if err != nil {
		return nil, err
	}

	// Fetch the breakdown from the analytics client (Postgres-backed).
	data, err := h.analyticsClient.QueryBreakdown(ctx, accountUUIDs, accountExternalIDsByProvider, start, end, dimension)
	if err != nil {
		return nil, fmt.Errorf("failed to query breakdown: %w", err)
	}

	return &BreakdownResponse{
		Dimension: dimension,
		Start:     start.Format(time.RFC3339),
		End:       end.Format(time.RFC3339),
		Data:      data,
	}, nil
}

// requestedAnalyticsAccounts returns the accounts an analytics request names.
// The plural account_ids (comma-separated UUIDs, validated and capped by
// parseAccountIDs) wins when both it and the legacy singular account_id are
// sent; the singular form stays for older clients and may be a raw external
// id. Empty means no account filter.
func requestedAnalyticsAccounts(params map[string]string) ([]string, error) {
	ids, err := parseAccountIDs(params["account_ids"])
	if err != nil {
		return nil, NewClientError(400, err.Error())
	}
	if len(ids) > 0 {
		return ids, nil
	}
	if id := params["account_id"]; id != "" {
		return []string{id}, nil
	}
	return nil, nil
}

// resolveAnalyticsAccountFilter enforces the session's allowed_accounts scope
// on the requested accounts and returns the union dual-column filter the
// analytics queries run with. Scoped users must name at least one account and
// every named account must be in their scope: one out-of-scope id refuses the
// whole request with the same 404 a single out-of-scope account_id gets, so
// the response neither narrows silently nor reveals whether the id exists.
// Admin/unrestricted sessions pass through (no accounts means all accounts).
//
// For a scoped session the scope decision and the filter come from the same
// cloud_accounts snapshot, so the filter always describes the accounts the
// decision allowed, even if another account is registered concurrently.
func (h *Handler) resolveAnalyticsAccountFilter(ctx context.Context, session *Session, accountIDs []string) (uuids []string, externalIDsByProvider map[string][]string, err error) {
	allowed, err := h.getAccountScope(ctx, session)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get allowed accounts: %w", err)
	}
	if allowed.AllowsAll() && len(accountIDs) == 0 {
		return nil, nil, nil
	}
	if len(accountIDs) == 0 {
		return nil, nil, NewClientError(400, "account_id is required for scoped users")
	}
	accounts, err := h.config.ListCloudAccounts(ctx, config.CloudAccountFilter{})
	if err != nil {
		if allowed.AllowsAll() {
			// Unrestricted: no worse than treating the ids as external ids.
			return nil, map[string][]string{"": accountIDs}, nil
		}
		return nil, nil, fmt.Errorf("failed to list cloud accounts: %w", err)
	}
	for _, id := range accountIDs {
		var u []string
		var e map[string][]string
		if allowed.AllowsAll() {
			u, e = unscopedAccountFilter(accounts, id)
		} else {
			var ok bool
			if u, e, ok = scopedAnalyticsAccountFilter(allowed, accounts, id); !ok {
				return nil, nil, errNotFound
			}
		}
		uuids, externalIDsByProvider = mergeAccountFilter(uuids, externalIDsByProvider, u, e)
	}
	return uuids, externalIDsByProvider, nil
}

// mergeAccountFilter unions one account's dual-column filter into the
// accumulated one, dropping duplicates.
func mergeAccountFilter(uuids []string, ext map[string][]string, addUUIDs []string, addExt map[string][]string) (mergedUUIDs []string, mergedExt map[string][]string) {
	for _, u := range addUUIDs {
		if !slices.Contains(uuids, u) {
			uuids = append(uuids, u)
		}
	}
	for provider, ids := range addExt {
		if ext == nil {
			ext = map[string][]string{}
		}
		for _, id := range ids {
			if !slices.Contains(ext[provider], id) {
				ext[provider] = append(ext[provider], id)
			}
		}
	}
	return uuids, ext
}

// scopedAnalyticsAccountFilter reports whether a restricted scope allows the
// requested account_id and, when it does, returns the filter for it:
//
//   - A registered account UUID is authoritative: the UUID plus the account's
//     external id under its own provider.
//   - Otherwise the value is a raw external id, which is unique only per
//     provider. When exactly one registered account has it, the id is
//     attributed to that account and the filter is scoped to the account's
//     provider, so an unregistered account of another provider that shares
//     the number (an AWS account id and a GCP project number, say) adds no
//     rows (issue #624). Legacy rows with an empty provider are not matched
//     either: nothing ties them to this account rather than to another
//     provider's, and the dashboard and inventory scope intersection
//     (intersectDualColumnScope) leaves them out the same way.
//   - An id that two or more registered accounts share cannot be attributed
//     and is refused, rather than read through a name that depends on account
//     list order (issue #520).
//   - An id no registered account has is judged on the literal allow-list
//     entry alone. There is no provider to pin it to, so it keeps the
//     unconstrained "" bucket: the grant itself names the bare id.
func scopedAnalyticsAccountFilter(allowed auth.AccountScope, accounts []config.CloudAccount, accountID string) (uuids []string, externalIDsByProvider map[string][]string, ok bool) {
	for i := range accounts {
		if accounts[i].ID == accountID {
			a := &accounts[i]
			if !allowed.Allows(a.ID, a.Name) {
				return nil, nil, false
			}
			return []string{a.ID}, providerScopedExternalID(a.Provider, a.ExternalID), true
		}
	}
	match, ambiguous := externalIDOwner(accounts, accountID)
	switch {
	case ambiguous:
		return nil, nil, false
	case match == nil:
		if !allowed.Allows(accountID, "") {
			return nil, nil, false
		}
		return nil, map[string][]string{"": {accountID}}, true
	case match.Provider == "" || !allowed.Allows(accountID, match.Name):
		// Out of scope, or a registered account with no provider (which the
		// cloud_accounts CHECK rules out on Postgres): its id could not be
		// scoped to a provider, so fail closed.
		return nil, nil, false
	}
	return nil, providerScopedExternalID(match.Provider, accountID), true
}

// externalIDOwner returns the one registered account whose external id is id,
// nil when none has it, or ambiguous when more than one does.
func externalIDOwner(accounts []config.CloudAccount, id string) (match *config.CloudAccount, ambiguous bool) {
	for i := range accounts {
		if accounts[i].ExternalID != id {
			continue
		}
		if match != nil {
			return nil, true
		}
		match = &accounts[i]
	}
	return match, false
}

// providerScopedExternalID returns the external-id half of the filter for an
// id under its provider, or nil when either is empty: an id with no provider
// would land in the unconstrained "" bucket and match every provider's rows.
func providerScopedExternalID(provider, externalID string) map[string][]string {
	if provider == "" || externalID == "" {
		return nil
	}
	return map[string][]string{provider: {externalID}}
}

// triggerAnalyticsCollection handles POST /analytics/collect (admin only)
// This can be used to manually trigger the hourly collection.
func (h *Handler) triggerAnalyticsCollection(ctx context.Context, req *events.LambdaFunctionURLRequest, _ map[string]string) (any, error) {
	// Admin-only operation.
	if _, err := h.requireAdmin(ctx, req); err != nil {
		return nil, err
	}
	if h.analyticsCollector == nil {
		return nil, fmt.Errorf("analytics collector not configured")
	}

	if err := h.analyticsCollector.Collect(ctx); err != nil {
		return nil, fmt.Errorf("collection failed: %w", err)
	}

	return map[string]string{
		"status":  "success",
		"message": "Analytics collection completed",
	}, nil
}

// parseDateRange parses start and end date strings with defaults.
func parseDateRange(startStr, endStr string) (time.Time, time.Time, error) { //nolint:gocritic // unnamedResult: return names would conflict with body locals
	var start, end time.Time
	var err error

	// Default end to now
	if endStr == "" {
		end = time.Now().UTC()
	} else {
		end, err = time.Parse(time.RFC3339, endStr)
		if err != nil {
			// Try date-only format
			end, err = time.Parse("2006-01-02", endStr)
			if err != nil {
				return time.Time{}, time.Time{}, NewClientError(400, "invalid end date format")
			}
			// Set to end of day
			end = end.Add(24*time.Hour - time.Second)
		}
	}

	// Default start to 7 days ago
	if startStr == "" {
		start = end.AddDate(0, 0, -7)
	} else {
		start, err = time.Parse(time.RFC3339, startStr)
		if err != nil {
			// Try date-only format
			start, err = time.Parse("2006-01-02", startStr)
			if err != nil {
				return time.Time{}, time.Time{}, NewClientError(400, "invalid start date format")
			}
		}
	}

	// Validate range order.
	if start.After(end) {
		return time.Time{}, time.Time{}, NewClientError(400, "start date must be before end date")
	}

	// Cap the range to at most 366 days to prevent full-table-scan DoS via
	// start=1970-01-01&end=2100-12-31 (issue #414).
	const maxRangeDays = 366
	if end.Sub(start) > maxRangeDays*24*time.Hour {
		return time.Time{}, time.Time{}, NewClientError(400,
			fmt.Sprintf("date range too large: maximum allowed range is %d days", maxRangeDays))
	}

	return start, end, nil
}
