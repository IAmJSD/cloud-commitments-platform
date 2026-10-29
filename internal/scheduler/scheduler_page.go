package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// ErrRecommendationsPageNeedsScan reports that PageRecommendations cannot
// reproduce the Go-side list filters for this request in SQL. The caller
// serves the request from ScanRecommendations instead, which applies them in
// Go exactly as before.
var ErrRecommendationsPageNeedsScan = errors.New("recommendations page needs the Go-side scan")

func needsScan(reason string) error {
	return fmt.Errorf("%w: %s", ErrRecommendationsPageNeedsScan, reason)
}

// PageRecommendations returns one page of the recommendations list, at most
// limit rows after afterID in byte-wise ID order, with the summary of the
// whole filtered set (issue #981). Unlike ScanRecommendations, which hands
// every row to the caller, the filters run in SQL: the store totals the
// summary in an aggregate query and returns only the page.
//
// The filters are the ones ScanRecommendations and the handler apply:
// scope's filter, enabled providers and account scope, plus suppressions and
// account overrides. Overrides are resolved here, for the (account,
// provider, service) keys the scope's rows have, with the same resolver as
// the scan. Rows on a suppressed 6-tuple are few (recent purchases), so the
// store hands them to applySuppressionIndex and the override filter here
// rather than re-implementing the variant match in SQL; every other row has
// no suppression and is filtered by the resolved overrides in SQL. The page
// rows are then run through the same Go filters once more, which also stamps
// their suppression badge fields; a row the Go filters would drop means SQL
// and Go disagree, and the request falls back rather than show it.
//
// It returns an error wrapping ErrRecommendationsPageNeedsScan when SQL
// cannot reproduce the Go filters for this request (override resolution
// failed, values SQL cannot compute like Go, duplicate IDs, a cursor that is
// not valid text); the caller then uses ScanRecommendations. The cold-start
// kick has already run by then, and the scan's own kick is a no-op (the
// collection marker or Lambda guard stops it). A suppression read failure
// fails the request, as it does for the scan (issue #111).
func (s *Scheduler) PageRecommendations(ctx context.Context, scope config.RecommendationListScope, afterID string, limit int) (*config.RecommendationPage, error) {
	freshness, err := s.startCacheRead(ctx)
	if err != nil {
		return nil, err
	}
	suppressions, err := s.loadSuppressionIndex(ctx)
	if err != nil {
		return nil, err
	}
	rules, resolved, err := s.resolvePageOverrides(ctx, scope)
	if err != nil {
		return nil, err
	}
	q := config.RecommendationPageQuery{
		Scope: scope, Rules: rules, SuppressionKeys: suppressionPageKeys(suppressions),
		AfterID: afterID, Limit: limit,
	}
	if len(q.SuppressionKeys) > 0 {
		q.DecideSuppressed = func(rows []config.StoredRecommendation) ([]string, error) {
			return decideSuppressedRows(rows, suppressions, resolved), nil
		}
	}
	page, err := s.config.PageStoredRecommendations(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("failed to page recommendations: %w", err)
	}
	if page.Fallback != "" {
		return nil, needsScan(page.Fallback)
	}
	rows, ok := filterPageRows(page.Rows, suppressions, resolved)
	if !ok {
		return nil, needsScan("SQL kept a row the Go filters drop")
	}
	page.Rows = rows
	s.finishCacheRead(ctx, freshness)
	return &page, nil
}

// resolvePageOverrides resolves account overrides for every key of the
// scope's rows and returns them as SQL rules plus the map the Go filter
// uses. A resolution failure, which the scan logs and passes through
// unfiltered chunk by chunk, falls back to the scan so it behaves the same.
// So do two rare shapes the rules cannot express: an empty but present
// cloud_account_id (resolution then depends on which row the scan meets
// first) and two keys joining to the same AccountConfigKey string.
func (s *Scheduler) resolvePageOverrides(ctx context.Context, scope config.RecommendationListScope) ([]config.RecommendationOverrideRule, map[string]*config.ServiceConfig, error) {
	triples, err := s.config.ListStoredRecommendationTriples(ctx, scope)
	if err != nil {
		return nil, nil, err
	}
	recs := make([]config.RecommendationRecord, 0, len(triples))
	rules := make([]config.RecommendationOverrideRule, 0, len(triples))
	keys := make(map[string]struct{}, len(triples))
	for _, t := range triples {
		if t.HasAccount && t.AccountID == "" {
			return nil, nil, needsScan("a recommendation has an empty cloud_account_id")
		}
		key := config.AccountConfigKey(t.AccountID, t.Provider, t.Service)
		if _, dup := keys[key]; dup {
			return nil, nil, needsScan("two override keys join to " + key)
		}
		keys[key] = struct{}{}
		rec := config.RecommendationRecord{Provider: t.Provider, Service: t.Service}
		if t.HasAccount {
			id := t.AccountID
			rec.CloudAccountID = &id
		}
		recs = append(recs, rec)
		rules = append(rules, config.RecommendationOverrideRule{
			Key: key, MinCountExempt: common.IsSavingsPlan(common.ServiceType(t.Service)),
		})
	}
	resolved, err := config.ResolveAccountConfigsForRecs(ctx, s.config, recs)
	if err != nil {
		logging.Warnf("recommendations page: failed to resolve account overrides, using the scan: %v", err)
		return nil, nil, needsScan("account override resolution failed")
	}
	for i := range rules {
		rules[i].Config = resolved[rules[i].Key]
	}
	return rules, resolved, nil
}

// suppressionPageKeys lists the suppression 6-tuples as the store matches
// rows against them: one key per stored spelling of each canonical service.
func suppressionPageKeys(index map[suppressionKey][]config.PurchaseSuppression) []string {
	var keys []string
	for k := range index {
		for _, service := range config.SuppressionServiceSpellings(k.provider, k.service) {
			keys = append(keys, config.SuppressionTupleKey(k.accountID, k.provider, service, k.region, k.resourceType, k.engine))
		}
	}
	sort.Strings(keys)
	return keys
}

// decideSuppressedRows returns the primary keys of the rows that survive
// suppressions and then account overrides, decided by the same functions
// the scan uses, one row at a time.
func decideSuppressedRows(rows []config.StoredRecommendation, index map[suppressionKey][]config.PurchaseSuppression, resolved map[string]*config.ServiceConfig) []string {
	keep := make([]string, 0, len(rows))
	for i := range rows {
		out := applySuppressionIndex([]config.RecommendationRecord{rows[i].Rec}, index)
		if len(out) == 1 && len(resolved) > 0 {
			out = filterRecsByResolvedConfigs(out, resolved)
		}
		if len(out) == 1 {
			keep = append(keep, rows[i].RowID)
		}
	}
	return keep
}

// filterPageRows runs the page through the scan's suppression and override
// filters, which stamps the suppression badge fields on the rows. ok is false
// when the filters drop a row SQL kept.
func filterPageRows(rows []config.RecommendationRecord, index map[suppressionKey][]config.PurchaseSuppression, resolved map[string]*config.ServiceConfig) ([]config.RecommendationRecord, bool) {
	out := rows
	if len(index) > 0 {
		out = applySuppressionIndex(out, index)
	}
	if len(resolved) > 0 && len(filterRecsByResolvedConfigs(out, resolved)) != len(out) {
		return nil, false
	}
	return out, len(out) == len(rows)
}
