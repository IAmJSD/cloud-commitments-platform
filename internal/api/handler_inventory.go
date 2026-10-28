// Package api provides the HTTP API handlers for the CUDly dashboard.
package api

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/analytics"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
)

// knownProviders is the ordered list of providers we always include in
// the coverage response so the frontend can rely on a stable ordering.
// Providers with no usage data get an empty Services slice and a nil
// OverallCoveragePct — the frontend renders "No usage detected" for them.
var knownProviders = []string{"aws", "azure", "gcp"}

// listActiveCommitments handles GET /api/inventory/commitments.
//
// Returns one row per *active* (non-expired) PurchaseHistoryRecord, with
// the account name joined in for display and expiry computed via the
// shared commitmentExpiry helper. Rows are sorted by EndDate ascending
// so the most-actionable (soonest-expiring) commitments float to the
// top — matches the dashboard's "what should I renew next?" framing
// without forcing the UI to re-sort client-side.
//
// Auth: `view:purchases`. Purchase history is the same source we already
// gate behind that permission for the /api/history page, so reusing the
// resource keeps the role matrix consistent. The session's
// allowed_accounts list is then applied via
// filterPurchaseHistoryByAllowedAccounts so a restricted-access user
// sees only the commitments belonging to accounts they're entitled to.
func (h *Handler) listActiveCommitments(ctx context.Context, req *events.LambdaFunctionURLRequest, params map[string]string) (any, error) {
	session, err := h.requirePermission(ctx, req, "view", "purchases")
	if err != nil {
		return nil, err
	}

	now := time.Now()
	purchases, err := h.fetchCommitmentRecords(ctx, now, session, params)
	if err != nil {
		return nil, err
	}

	purchases, err = h.filterPurchaseHistoryByAllowedAccounts(ctx, session, purchases)
	if err != nil {
		return nil, err
	}

	accounts, listErr := h.config.ListCloudAccounts(ctx, config.CloudAccountFilter{})
	if listErr != nil {
		return nil, fmt.Errorf("failed to list cloud accounts: %w", listErr)
	}

	commitments := make([]InventoryCommitment, 0, len(purchases))
	for _rvc := range purchases {
		p := purchases[_rvc]
		if !isActiveCommitment(p, now) {
			continue
		}
		commitments = append(commitments, buildInventoryCommitment(p, historyRowAccountName(accounts, p)))
	}

	// Soonest-expiring first. The dashboard framing is "what do I need to
	// renew next" — surfacing the imminent end_date on top means the
	// frontend doesn't need to re-sort and the user's eye lands on the
	// most-actionable rows immediately.
	sort.SliceStable(commitments, func(i, j int) bool {
		return commitments[i].EndDate.Before(commitments[j].EndDate)
	})

	return InventoryCommitmentsResponse{Commitments: commitments}, nil
}

// fetchCommitmentRecords reads the active purchase_history rows from the
// store, honoring optional `account_id` and `provider` query params the same
// way fetchPurchaseHistory does for /api/history. The read goes through
// GetActivePurchaseHistory so the active filter runs in SQL with no row cap:
// a newest-first LIMIT page dropped exactly the oldest still-active 1y/3y
// commitments once history exceeded the cap, silently hiding rows the user is
// entitled to (issue #1140). The result is bounded by the number of live
// commitments, not by all history ever recorded.
//
// The singular `account_id` (a top-bar chip cloud_accounts UUID for current
// callers, or a raw external number for legacy ones) is resolved to the
// dual-column filter inputs so a commitment row that carries only the external
// account_id (cloud_account_id NULL) is still returned — the original
// #701/#498/#866 bug was that passing the UUID to the single-column
// GetPurchaseHistory (account_id) matched nothing.
//
// The filter is then narrowed to the session's allowed_accounts by
// intersectAccountFilterScope, the same contract as the dashboard KPI path
// (issues #956, #384): a restricted session's scope is pushed into the SQL
// read, and an explicit account_id outside it is dropped rather than queried.
// An empty intersection resolves to the non-nil-but-empty sentinel and
// short-circuits to an empty result without querying (an empty scope sent to
// the store would match ALL rows, not none). Unrestricted / admin sessions
// keep the caller's filter, or the all-accounts read without one. The
// in-memory filterPurchaseHistoryByAllowedAccounts in the callers still runs
// afterwards as defense in depth.
//
// `provider` filtering is applied in-memory after the store read so the
// record set is small enough that a post-read filter has negligible cost.
func (h *Handler) fetchCommitmentRecords(ctx context.Context, asOf time.Time, session *Session, params map[string]string) ([]config.PurchaseHistoryRecord, error) {
	provider, err := providerFilterParam(params["provider"])
	if err != nil {
		return nil, err
	}
	filterUUIDs, filterExternalIDsByProvider := h.resolveSingleAccountFilterIDs(ctx, params["account_id"])
	uuids, externalIDsByProvider, err := h.intersectAccountFilterScope(ctx, session, filterUUIDs, filterExternalIDsByProvider)
	if err != nil {
		return nil, err
	}
	if uuids != nil && len(uuids) == 0 && len(externalIDsByProvider) == 0 {
		return nil, nil
	}
	rows, err := h.config.GetActivePurchaseHistory(ctx, asOf, uuids, externalIDsByProvider)
	if err != nil {
		return nil, err
	}

	// Apply provider filter in-memory. An absent param or "all" means
	// "all providers"; providerFilterParam refused anything but the
	// lowercase names the store holds (aws, azure, gcp).
	if provider != "" {
		filtered := rows[:0]
		for _rvc := range rows {
			r := rows[_rvc]
			if r.Provider == provider {
				filtered = append(filtered, r)
			}
		}
		rows = filtered
	}

	return rows, nil
}

// buildInventoryCommitment maps a PurchaseHistoryRecord to the
// response-layer InventoryCommitment. The ID is namespaced by account so
// the JSON payload is globally unique without a DB schema change —
// purchase_id alone is only unique within an account.
func buildInventoryCommitment(p config.PurchaseHistoryRecord, accountName string) InventoryCommitment {
	return InventoryCommitment{
		ID:               p.AccountID + ":" + p.PurchaseID,
		Provider:         p.Provider,
		AccountID:        p.AccountID,
		AccountName:      accountName,
		Service:          p.Service,
		ResourceType:     p.ResourceType,
		Region:           p.Region,
		Count:            p.Count,
		TermYears:        p.Term,
		PaymentOption:    p.Payment,
		StartDate:        p.Timestamp,
		EndDate:          commitmentExpiry(p),
		UpfrontCost:      p.UpfrontCost,
		MonthlyCost:      p.MonthlyCost,
		EstimatedSavings: p.EstimatedSavings,
		Status:           "active",
	}
}

// getCoverageBreakdown handles GET /api/inventory/coverage.
//
// Returns per-provider, per-service coverage breakdowns computed from
// two data sources already available in the system:
//   - Active commitments (purchase history): their effective covered
//     monthly spend (recurring MonthlyCost plus amortized upfront — see
//     commitmentCoveredMonthly) is the "covered" portion of monthly spend.
//   - Recommendations (scheduler): their Savings represent the remaining
//     on-demand gap that could still be committed.
//
// Coverage% per service = covered / (covered + on_demand).
// A provider with no data in either source gets Services=nil and
// OverallCoveragePct=nil — the frontend renders "No usage detected".
//
// Auth: `view:purchases`. Same gate as /api/inventory/commitments;
// both pull from purchase history so the role is consistent.
func (h *Handler) getCoverageBreakdown(ctx context.Context, req *events.LambdaFunctionURLRequest, params map[string]string) (any, error) {
	session, err := h.requirePermission(ctx, req, "view", "purchases")
	if err != nil {
		return nil, err
	}
	provider, err := providerFilterParam(params["provider"])
	if err != nil {
		return nil, err
	}

	// --- covered: active commitments ----------------------------------------
	now := time.Now()
	purchases, err := h.fetchCommitmentRecords(ctx, now, session, params)
	if err != nil {
		return nil, err
	}
	purchases, err = h.filterPurchaseHistoryByAllowedAccounts(ctx, session, purchases)
	if err != nil {
		return nil, err
	}

	// coveredByKey accumulates the effective covered monthly spend by
	// "provider:service". A commitment's covered monthly is its recurring
	// MonthlyCost plus the amortized upfront, so an all-upfront commitment
	// (MonthlyCost nil, UpfrontCost > 0 — typical for Azure RIs) still
	// registers as covered instead of being silently dropped (issue: Azure
	// showed $0 coverage while the dashboard reported active commitments).
	coveredByKey := make(map[string]*float64)
	for _rvc := range purchases {
		p := purchases[_rvc]
		if !isActiveCommitment(p, now) {
			continue
		}
		key := p.Provider + ":" + p.Service
		cost := commitmentCoveredMonthly(p)
		total, exists := coveredByKey[key]
		if !exists {
			total = new(float64)
		}
		coveredByKey[key] = sumKnownCosts(total, cost)
	}

	// --- on-demand gap: recommendations -------------------------------------
	recs, err := h.fetchCoverageOnDemandRecs(ctx, session, params)
	if err != nil {
		return nil, err
	}
	onDemandByKey := aggregateOnDemandByKey(recs, provider)

	return buildCoverageBreakdown(coveredByKey, onDemandByKey), nil
}

// fetchCoverageOnDemandRecs reads the recommendations behind the coverage
// breakdown's on-demand gap. Recommendations represent uncommitted demand
// that could be purchased; their Savings field is the monthly on-demand cost
// of the uncovered gap. Recs are scoped to the account chip the same way
// fetchCommitmentRecords scopes commitments; otherwise the covered side honors
// the chip but the on-demand side bleeds in other accounts' gaps, producing
// misleading per-service coverage (issue #866 follow-up: CR pass on PR #881).
//
// The session's account scope is resolved once and used both to decide
// whether this read may kick a background collection (issue #627) and to
// post-filter the recs, so restricted users cannot infer data from accounts
// they are not entitled to.
func (h *Handler) fetchCoverageOnDemandRecs(ctx context.Context, session *Session, params map[string]string) ([]config.RecommendationRecord, error) {
	scope, err := h.getAccountScope(ctx, session)
	if err != nil {
		return nil, fmt.Errorf("failed to get allowed accounts: %w", err)
	}
	recs, err := h.scheduler.ListRecommendations(recommendationsReadContext(ctx, scope), buildCoverageRecFilter(params))
	if err != nil {
		// Non-fatal: recommendations are best-effort for coverage display.
		// An empty rec list is treated as "no uncovered gap": coverage
		// shows covered-only totals rather than failing the whole page.
		return nil, nil
	}
	return h.filterRecommendationsByScope(ctx, scope, recs)
}

// buildCoverageRecFilter translates the query-string chip params into a
// RecommendationFilter for ListRecommendations. Currently scopes by
// account_id only — provider filtering is applied in aggregateOnDemandByKey
// because the response envelope always includes the full known-providers
// list (a per-provider filter at fetch time would still need an in-memory
// pass to enumerate the other providers as "no usage detected").
//
// Extracted from getCoverageBreakdown to keep that function under the
// gocyclo budget after PR #881's extraction.
func buildCoverageRecFilter(params map[string]string) config.RecommendationFilter {
	filter := config.RecommendationFilter{}
	if accountID := params["account_id"]; accountID != "" {
		filter.AccountIDs = []string{accountID}
	}
	return filter
}

// aggregateOnDemandByKey builds the "provider:service" → monthly-savings map
// from a recommendation slice, applying an optional provider filter.
// Pulled out of getCoverageBreakdown to keep that function under the
// cyclomatic limit.
func aggregateOnDemandByKey(recs []config.RecommendationRecord, providerFilter string) map[string]float64 {
	out := make(map[string]float64)
	for _rvc := range recs {
		rec := recs[_rvc]
		if providerFilter != "" && rec.Provider != providerFilter {
			continue
		}
		out[rec.Provider+":"+rec.Service] += rec.Savings
	}
	return out
}

// commitmentCoveredMonthly returns the effective covered monthly spend of a
// single active commitment: its recurring MonthlyCost (when present) plus the
// upfront amortized over the term. This mirrors the canonical effective-monthly
// formula used elsewhere in the codebase (analytics.Collector amortizes
// UpfrontCost/(Term*MonthsPerYear); exchange_lookup adds MonthlyCost +
// UpfrontCost/termMonths) so the Coverage tab and the savings analytics agree
// on what "covered" means.
//
// MonthlyCost is *float64 because the provider API leaves it nil for
// all-upfront commitments where there is no recurring charge (Azure RIs in
// particular — see config.PurchaseHistoryRecord.MonthlyCost). The previous
// Coverage code skipped those rows entirely, so an Azure subscription whose
// commitments are all upfront rendered as $0 / "No usage detected" even though
// the dashboard counted the same commitments. A nil MonthlyCost is treated as a
// real $0 recurring component (not a fabricated total) and the upfront still
// contributes its amortized share, so the covered figure is never silently 0.
//
// Term <= 0 cannot be amortized (division by zero); such a row contributes only
// its recurring MonthlyCost. The scheduler only writes Term >= 1 rows, so this
// guard matches analytics.Collector's skip-bad-term defense rather than papering
// over real data.
func commitmentCoveredMonthly(p config.PurchaseHistoryRecord) *float64 {
	if p.UpfrontCost == nil {
		return nil
	}
	var covered float64
	if p.MonthlyCost != nil {
		covered += *p.MonthlyCost
	}
	if p.Term > 0 {
		covered += *p.UpfrontCost / (float64(p.Term) * analytics.MonthsPerYear)
	}
	return &covered
}

// buildCoverageBreakdown constructs the CoverageBreakdownResponse from
// the two pre-aggregated maps. Extracted so it can be unit-tested without
// requiring a full Handler.
//
// coveredByKey and onDemandByKey are both keyed by "provider:service".
// A provider that appears in neither map gets Services=nil and
// OverallCoveragePct=nil (no usage detected).
func buildCoverageBreakdown(
	coveredByKey map[string]*float64,
	onDemandByKey map[string]float64,
) CoverageBreakdownResponse {
	// Collect all (provider, service) keys appearing in either map.
	type providerService struct{ provider, service string }
	keySet := make(map[providerService]struct{})
	for k := range coveredByKey {
		provider, service := splitProviderService(k)
		keySet[providerService{provider, service}] = struct{}{}
	}
	for k := range onDemandByKey {
		provider, service := splitProviderService(k)
		keySet[providerService{provider, service}] = struct{}{}
	}

	// Group by provider, then build per-service rows.
	providerSvcMap := make(map[string][]CoverageServiceRow)
	for ps := range keySet {
		key := ps.provider + ":" + ps.service
		covered, exists := coveredByKey[key]
		if !exists {
			covered = new(float64)
		}
		onDemand := onDemandByKey[key]
		row := CoverageServiceRow{
			Service:         ps.service,
			CoveredMonthly:  covered,
			OnDemandMonthly: onDemand,
			CoveragePct:     coveragePct(covered, onDemand),
		}
		providerSvcMap[ps.provider] = append(providerSvcMap[ps.provider], row)
	}

	// Sort service rows within each provider for stable output.
	for p := range providerSvcMap {
		rows := providerSvcMap[p]
		sort.SliceStable(rows, func(i, j int) bool {
			return rows[i].Service < rows[j].Service
		})
		providerSvcMap[p] = rows
	}

	// Emit one section per known provider in canonical order; providers
	// with no data get nil Services and nil OverallCoveragePct so the
	// frontend can show "No usage detected" rather than an empty table.
	sections := make([]ProviderCoverageSection, 0, len(knownProviders))
	for _, p := range knownProviders {
		rows := providerSvcMap[p]
		if len(rows) == 0 {
			sections = append(sections, ProviderCoverageSection{
				Provider:           p,
				Services:           nil,
				OverallCoveragePct: nil,
			})
			continue
		}

		totalCovered := new(float64)
		var totalOnDemand float64
		for _, r := range rows {
			totalCovered = sumKnownCosts(totalCovered, r.CoveredMonthly)
			totalOnDemand += r.OnDemandMonthly
		}
		sections = append(sections, ProviderCoverageSection{
			Provider:           p,
			Services:           rows,
			OverallCoveragePct: coveragePct(totalCovered, totalOnDemand),
		})
	}

	return CoverageBreakdownResponse{Providers: sections}
}

// coveragePct returns covered / (covered + onDemand) * 100 as a *float64.
// Returns nil when both inputs are zero (no usage signal) to preserve the
// "absent" semantic per feedback_nullable_not_zero — callers should render
// nil as "N/A", not "0%".
func coveragePct(covered *float64, onDemand float64) *float64 {
	if covered == nil {
		return nil
	}
	total := *covered + onDemand
	if total == 0 {
		return nil
	}
	pct := *covered / total * 100
	return &pct
}

// splitProviderService splits a "provider:service" key back into its two
// parts. The separator is always the first colon so service names
// containing colons (unlikely but possible) are preserved.
func splitProviderService(key string) (provider, service string) {
	for i := 0; i < len(key); i++ {
		if key[i] == ':' {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}
