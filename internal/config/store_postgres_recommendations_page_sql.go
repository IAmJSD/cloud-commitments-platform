package config

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SQL builders for PageStoredRecommendations (issue #981). Every expression
// reads the decoded payload the way the Go filters do: a missing or JSON-null
// string field is "".
//
// The per-row work is staged in two subqueries, each fenced with OFFSET 0 so
// the planner cannot inline it: a reads each payload field once into a
// column, b derives the filter decisions from a's columns. Inlining would
// substitute every field expression into every decision and multiply the
// expression size (and per-row cost) a hundredfold. See pageStages for how
// the aggregate and the page read join the stages.
//
// The expressions must never raise an error on a row the Go path would have
// handled, because a single bad row would then fail every page. Numbers are
// therefore type-checked with jsonb_typeof before any cast, and float
// arithmetic only runs on magnitudes where it cannot overflow or underflow
// (Postgres raises on both, Go does not). A row outside those bounds is
// reported as "unsure" and the whole request falls back to the Go scan.

const (
	pageAccountExpr  = `COALESCE(r.payload->>'cloud_account_id', '')`
	pageProviderExpr = `COALESCE(r.payload->>'provider', '')`
	pageServiceExpr  = `COALESCE(r.payload->>'service', '')`
	pageRegionExpr   = `COALESCE(r.payload->>'region', '')`
	pageTypeExpr     = `COALESCE(r.payload->>'resource_type', '')`
	pageEngineExpr   = `COALESCE(r.payload->>'engine', '')`
	// pageKeyExpr is recommendationScanKeyExpr on the aliased table, so the
	// page read can walk recommendations_scan_key_idx.
	pageKeyExpr = `(COALESCE(r.payload->>'id', '') COLLATE "C")`
)

// pageNumericField is a payload number the filters read; stage a holds it as
// t_<name> (its jsonb_typeof) and n_<name> (numeric, NULL unless a number).
type pageNumericField struct{ name, field string }

// pageSummaryFields are read for every request, pagePctFields only for one
// with a MinSavingsPct floor.
var (
	pageSummaryFields = []pageNumericField{{"s", "savings"}, {"up", "upfront_cost"}, {"cnt", "count"}}
	pagePctFields     = []pageNumericField{{"sp", "savings_percentage"}, {"term", "term"}, {"od", "on_demand_cost"}, {"mc", "monthly_cost"}}
)

// pct states of the stage-b pct column.
const (
	pagePctDrop   = 1
	pagePctUnsure = 2
)

// pageArgs numbers query parameters as they are added.
type pageArgs struct{ args []any }

func (p *pageArgs) add(v any) string {
	p.args = append(p.args, v)
	return fmt.Sprintf("$%d", len(p.args))
}

// pageStageACols selects each payload value the filters need once. rules
// and touchKeys are parameter references (touchKeys is "" when there are no
// suppression keys); withPct adds the pct floor's inputs.
func pageStageACols(rules, touchKeys string, withPct bool) string {
	cols := make([]string, 0, 10+2*len(pageSummaryFields)+2*len(pagePctFields))
	cols = append(cols,
		"r.id AS rid", pageKeyExpr+" AS sk",
		pageAccountExpr+" AS acct", pageProviderExpr+" AS prov", pageServiceExpr+" AS svc",
		pageRegionExpr+" AS reg", pageTypeExpr+" AS rtype", pageEngineExpr+" AS eng",
		fmt.Sprintf(`%s::jsonb -> (%s || '|' || %s || '|' || %s) AS rule`, rules, pageAccountExpr, pageProviderExpr, pageServiceExpr),
	)
	fields := pageSummaryFields
	if withPct {
		fields = append(append([]pageNumericField(nil), fields...), pagePctFields...)
	}
	for _, f := range fields {
		cols = append(cols,
			fmt.Sprintf(`COALESCE(jsonb_typeof(r.payload->'%s'), 'null') AS t_%s`, f.field, f.name),
			fmt.Sprintf(`CASE WHEN jsonb_typeof(r.payload->'%[1]s') = 'number' THEN (r.payload->'%[1]s')::numeric END AS n_%[2]s`, f.field, f.name))
	}
	touched := "false"
	if touchKeys != "" {
		touched = pageTouchedExpr(touchKeys)
	}
	return strings.Join(append(cols, touched+" AS touched"), ", ")
}

// pageTouchedExpr is true for rows on one of the suppression 6-tuples.
func pageTouchedExpr(touchKeys string) string {
	return fmt.Sprintf(`(%s || E'\x1f' || %s || E'\x1f' || %s || E'\x1f' || %s || E'\x1f' || %s || E'\x1f' || %s) = ANY(%s::text[])`,
		pageAccountExpr, pageProviderExpr, pageServiceExpr, pageRegionExpr, pageTypeExpr, pageEngineExpr, touchKeys)
}

// pageSafe is true when the numeric x is 0 or within magnitudes where the
// float8 cast and the pct arithmetic can neither overflow nor underflow.
func pageSafe(x string) string {
	return fmt.Sprintf(`(%[1]s = 0 OR abs(%[1]s) BETWEEN 1e-100 AND 1e100)`, x)
}

const (
	pageSavings = `COALESCE(a.n_s, 0)`
	pageUpfront = `COALESCE(a.n_up, 0)`
	// pageCount is the payload count as Go decodes it (absent or null is 0);
	// a count that is not a plain integer is unsure.
	pageCount = `(CASE WHEN a.n_cnt = trunc(a.n_cnt) AND abs(a.n_cnt) <= 1e15 THEN a.n_cnt::bigint ELSE 0 END)`
)

// pageBadType is true when a numeric payload field holds a non-number, which
// the Go decoder rejects.
func pageBadType(name string) string {
	return fmt.Sprintf(`a.t_%s NOT IN ('number', 'null')`, name)
}

// pageNumbersUnsure flags rows whose summed or compared numbers SQL cannot
// handle like Go: a non-number in a numeric field, a savings or upfront value
// outside pageSafe, or a count that is not a plain integer.
var pageNumbersUnsure = `(` + strings.Join([]string{
	pageBadType("s"), pageBadType("up"), pageBadType("cnt"),
	`NOT ` + pageSafe(pageSavings), `NOT ` + pageSafe(pageUpfront),
	`(a.n_cnt IS NOT NULL AND (a.n_cnt <> trunc(a.n_cnt) OR abs(a.n_cnt) > 1e15))`,
}, " OR ") + `)`

// pagePctState mirrors decodeStoredRecommendation's MinSavingsPct floor
// (recEffectiveSavingsPct and recOnDemandBaseline) in float8, operation for
// operation, so the rounding matches Go's: 1 where Go drops the row, 0 where
// it keeps it and 2 where the inputs are outside the bounds SQL can compute
// safely. floor is a float8 parameter reference.
func pagePctState(floor string) string {
	f8 := func(x string) string { return "(" + x + ")::float8" }
	onDemand := fmt.Sprintf(`((%s + %s) + %s / ((a.n_term)::bigint * 12)::float8)`, f8("a.n_mc"), f8(pageSavings), f8(pageUpfront))
	bad := strings.Join([]string{
		pageBadType("sp"), pageBadType("term"), pageBadType("od"), pageBadType("mc"), pageBadType("s"), pageBadType("up"),
	}, " OR ")
	return fmt.Sprintf(`CASE
	WHEN %[1]s THEN 2
	WHEN a.n_sp IS NOT NULL THEN (CASE WHEN NOT %[2]s THEN 2 WHEN %[3]s < %[4]s THEN 1 ELSE 0 END)
	WHEN COALESCE(a.n_term, 0) = 0 THEN 0
	WHEN a.n_term <> trunc(a.n_term) OR abs(a.n_term) > 1e9 THEN 2
	WHEN a.n_od > 0 THEN (CASE WHEN NOT %[5]s OR NOT %[6]s THEN 2 WHEN (%[7]s / %[8]s) * 100::float8 < %[4]s THEN 1 ELSE 0 END)
	WHEN a.n_mc IS NULL THEN 0
	WHEN a.prov = 'aws' THEN 0
	WHEN NOT %[9]s OR NOT %[6]s OR NOT %[10]s THEN 2
	WHEN %[11]s = 0 THEN 0
	WHEN (%[7]s / %[11]s) * 100::float8 < %[4]s THEN 1
	ELSE 0
END`,
		bad, pageSafe("a.n_sp"), f8("a.n_sp"), floor,
		pageSafe("a.n_od"), pageSafe(pageSavings), f8(pageSavings), f8("a.n_od"),
		pageSafe("a.n_mc"), pageSafe(pageUpfront), onDemand)
}

// pageInList mirrors inListRule: exclude wins, an empty include list allows
// anything else.
func pageInList(value, include, exclude string) string {
	return fmt.Sprintf(`(NOT COALESCE((a.rule->'%[3]s') ? %[1]s, false) AND (jsonb_array_length(a.rule->'%[2]s') = 0 OR COALESCE((a.rule->'%[2]s') ? %[1]s, false)))`,
		value, include, exclude)
}

// pageOverridePass mirrors passesOverrideDimensions under the row's resolved
// configuration: true when the key has no configuration and NULL when it was
// never resolved.
var pageOverridePass = fmt.Sprintf(`CASE WHEN NOT (a.rule->>'c')::boolean THEN true ELSE (
	(a.rule->>'en')::boolean
	AND (a.eng = '' OR %s)
	AND %s
	AND %s
	AND ((a.rule->>'sp')::boolean OR (a.rule->>'mc')::bigint <= 0 OR %s >= (a.rule->>'mc')::bigint)
) END`,
	pageInList("a.eng", "ie", "ee"), pageInList("a.reg", "ir", "er"), pageInList("a.rtype", "it", "et"), pageCount)

// pageStageBCols derives the decisions from stage a: pct (the floor's
// state), numunsure, pass (the override filter), and the float8 summands.
// floor is "" when there is no MinSavingsPct floor.
func pageStageBCols(floor string) string {
	pct := "0"
	if floor != "" {
		pct = pagePctState(floor)
	}
	return fmt.Sprintf(`a.rid, a.sk, a.reg, a.touched, a.rule, %s AS pct, %s AS numunsure, %s AS pass,
	CASE WHEN %s THEN (%s)::float8 ELSE 0 END AS sv,
	CASE WHEN %s THEN (%s)::float8 ELSE 0 END AS upv`,
		pct, pageNumbersUnsure, pageOverridePass,
		pageSafe(pageSavings), pageSavings, pageSafe(pageUpfront), pageUpfront)
}

// pageRuleJSON is one resolved override configuration as the SQL filter reads
// it. Lists are never null so the jsonb ? checks and lengths are defined.
type pageRuleJSON struct {
	Configured     bool     `json:"c"`
	Enabled        bool     `json:"en"`
	IncludeEngines []string `json:"ie"`
	ExcludeEngines []string `json:"ee"`
	IncludeRegions []string `json:"ir"`
	ExcludeRegions []string `json:"er"`
	IncludeTypes   []string `json:"it"`
	ExcludeTypes   []string `json:"et"`
	MinCount       int      `json:"mc"`
	MinCountExempt bool     `json:"sp"`
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// pageRulesJSON encodes rules as one JSON object keyed by AccountConfigKey,
// the same key the Go filter looks configurations up by.
func pageRulesJSON(rules []RecommendationOverrideRule) ([]byte, error) {
	obj := make(map[string]pageRuleJSON, len(rules))
	for _, rule := range rules {
		r := pageRuleJSON{}
		if c := rule.Config; c != nil {
			r = pageRuleJSON{
				Configured: true, Enabled: c.Enabled,
				IncludeEngines: nonNilStrings(c.IncludeEngines), ExcludeEngines: nonNilStrings(c.ExcludeEngines),
				IncludeRegions: nonNilStrings(c.IncludeRegions), ExcludeRegions: nonNilStrings(c.ExcludeRegions),
				IncludeTypes: nonNilStrings(c.IncludeTypes), ExcludeTypes: nonNilStrings(c.ExcludeTypes),
				MinCount: c.MinCount, MinCountExempt: rule.MinCountExempt,
			}
		}
		obj[rule.Key] = r
	}
	return json.Marshal(obj)
}

// pageScopeCond is the scope's row filter: the query-string filter on the
// table columns (shared with ListStoredRecommendations), then the enabled
// providers and the account scope on the payload. It must be the first thing
// added to p, because buildRecommendationFilter numbers its parameters from
// $1.
func pageScopeCond(p *pageArgs, scope RecommendationListScope) string {
	where, args := buildRecommendationFilter(scope.Filter)
	conds := []string{"TRUE"}
	if where != "" {
		conds[0] = strings.TrimPrefix(where, " WHERE ")
		p.args = append(p.args, args...)
	}
	if len(scope.EnabledProviders) > 0 {
		conds = append(conds, pageProviderExpr+" = ANY("+p.add(scope.EnabledProviders)+"::text[])")
	}
	if scope.Restricted {
		conds = append(conds, "r.payload->>'cloud_account_id' = ANY("+p.add(nonNilStrings(scope.AllowedAccountIDs))+"::text[])")
	}
	return "(" + strings.Join(conds, " AND ") + ")"
}

// pageStages are the staged row sources shared by the aggregate and the page
// read, built against one parameter list. The aggregate streams every row
// through nested subqueries (from); the page read joins the stages per row
// as LATERAL subqueries (lateral), so the planner can walk
// recommendations_scan_key_idx in order and stop at the LIMIT instead of
// staging and sorting every row. keep and unsure read stage b.
type pageStages struct {
	scope   string // the scope's row filter, on r
	from    string // "(stage b over stage a over r) b" for the aggregate
	lateral string // "r CROSS JOIN LATERAL a CROSS JOIN LATERAL b" for the page
	keep    string // the row survives every filter (NULL when unsure)
	unsure  string // SQL cannot decide the row the way Go does
}

func buildPageStages(p *pageArgs, q *RecommendationPageQuery, rules []byte, keepRowIDs []string) pageStages {
	scope := pageScopeCond(p, q.Scope)
	floor := ""
	if q.Scope.Filter.MinSavingsPct > 0 {
		floor = p.add(q.Scope.Filter.MinSavingsPct) + "::float8"
	}
	rulesParam := p.add(string(rules))
	touchKeys, override := "", "b.pass"
	if len(q.SuppressionKeys) > 0 {
		touchKeys = p.add(q.SuppressionKeys)
		override = fmt.Sprintf(`(CASE WHEN b.touched THEN b.rid = ANY(%s::uuid[]) ELSE b.pass END)`, p.add(nonNilStrings(keepRowIDs)))
	}
	aCols, bCols := pageStageACols(rulesParam, touchKeys, floor != ""), pageStageBCols(floor)
	return pageStages{
		scope: scope,
		from: fmt.Sprintf(`(SELECT %s FROM (SELECT %s FROM recommendations r WHERE %s OFFSET 0) a OFFSET 0) b`,
			bCols, aCols, scope),
		lateral: fmt.Sprintf(`recommendations r CROSS JOIN LATERAL (SELECT %s OFFSET 0) a CROSS JOIN LATERAL (SELECT %s OFFSET 0) b`,
			aCols, bCols),
		keep:   fmt.Sprintf(`(b.pct <> %d AND %s)`, pagePctDrop, override),
		unsure: fmt.Sprintf(`(b.pct = %d OR b.rule IS NULL OR b.numunsure)`, pagePctUnsure),
	}
}

// buildPageAggregateQuery totals the whole filtered set. The sums are ordered
// aggregates in scan order, so float8 addition happens in the order the Go
// summary used; count(DISTINCT) exposes duplicate IDs, which the Go path
// rejects.
func buildPageAggregateQuery(q *RecommendationPageQuery, rules []byte, keepRowIDs []string) (query string, args []any) {
	p := &pageArgs{}
	e := buildPageStages(p, q, rules, keepRowIDs)
	query = fmt.Sprintf(`SELECT
	count(*) FILTER (WHERE u),
	count(*) FILTER (WHERE k),
	count(DISTINCT sk) FILTER (WHERE k),
	COALESCE(sum(sv ORDER BY sk, rid) FILTER (WHERE k), 0),
	COALESCE(sum(upv ORDER BY sk, rid) FILTER (WHERE k), 0),
	COALESCE(array_agg(DISTINCT reg ORDER BY reg) FILTER (WHERE k AND reg <> ''), '{}')
FROM (
	SELECT b.rid, b.sk, b.sv, b.upv, b.reg, %s AS u, (NOT %s AND COALESCE(%s, false)) AS k
	FROM %s
	OFFSET 0
) d`,
		e.unsure, e.unsure, e.keep, e.from)
	return query, p.args
}

// buildPageRowsQuery reads the page: at most limit surviving rows after
// afterID in scan order.
func buildPageRowsQuery(q *RecommendationPageQuery, rules []byte, keepRowIDs []string) (query string, args []any) {
	p := &pageArgs{}
	e := buildPageStages(p, q, rules, keepRowIDs)
	where := e.scope + " AND COALESCE(" + e.keep + ", false)"
	if q.AfterID != "" {
		where += " AND " + pageKeyExpr + " > " + p.add(q.AfterID)
	}
	query = fmt.Sprintf(`SELECT r.id::text, r.payload FROM %s WHERE %s ORDER BY %s, r.id LIMIT %s`,
		e.lateral, where, pageKeyExpr, p.add(q.Limit))
	return query, p.args
}

// buildPageTouchedQuery reads the in-scope rows on a suppression 6-tuple.
func buildPageTouchedQuery(q *RecommendationPageQuery) (query string, args []any) {
	p := &pageArgs{}
	scope := pageScopeCond(p, q.Scope)
	keys := p.add(q.SuppressionKeys)
	return fmt.Sprintf(`SELECT r.id::text, r.payload FROM recommendations r WHERE %s AND %s`, scope, pageTouchedExpr(keys)), p.args
}

// buildPageTriplesQuery lists the distinct override keys of the scope's rows.
func buildPageTriplesQuery(scope RecommendationListScope) (query string, args []any) {
	p := &pageArgs{}
	cond := pageScopeCond(p, scope)
	return fmt.Sprintf(`SELECT DISTINCT %s, COALESCE(jsonb_typeof(r.payload->'cloud_account_id'), 'null') <> 'null', %s, %s FROM recommendations r WHERE %s`,
		pageAccountExpr, pageProviderExpr, pageServiceExpr, cond), p.args
}
