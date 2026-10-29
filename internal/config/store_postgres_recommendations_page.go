package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// ListStoredRecommendationTriples returns the distinct (account, provider,
// service) combinations of the rows scope selects, read from the payload,
// so the caller can resolve account overrides for exactly those keys before
// PageStoredRecommendations (issue #981).
func (s *PostgresStore) ListStoredRecommendationTriples(ctx context.Context, scope RecommendationListScope) ([]RecommendationTriple, error) {
	if err := s.checkAzurePricingMigration(ctx, scope.Filter.RequireAzurePricingMigration); err != nil {
		return nil, err
	}
	query, args := buildPageTriplesQuery(scope)
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query recommendation override keys: %w", err)
	}
	defer rows.Close()
	var out []RecommendationTriple
	for rows.Next() {
		var t RecommendationTriple
		if err := rows.Scan(&t.AccountID, &t.HasAccount, &t.Provider, &t.Service); err != nil {
			return nil, fmt.Errorf("failed to scan recommendation override key: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recommendation override keys iteration: %w", err)
	}
	return out, nil
}

// PageStoredRecommendations reads one page of the recommendations list with
// every list filter evaluated in SQL (issue #981): the query-string filter,
// the MinSavingsPct floor, the enabled providers, the account scope, account
// overrides (q.Rules) and suppressions (rows on q.SuppressionKeys are decided
// by q.DecideSuppressed). It returns the summary of the whole filtered set
// from one aggregate query and at most q.Limit rows after q.AfterID, so the
// rows handed to Go are the page, not the tenant.
//
// All reads run in one REPEATABLE READ, READ ONLY transaction: the rows
// DecideSuppressed sees, the summary and the page come from the same
// snapshot, so a collection committing mid-request cannot make the page
// disagree with its summary.
//
// When SQL cannot reproduce the Go filters faithfully the page comes back
// with Fallback set instead (see RecommendationPage).
func (s *PostgresStore) PageStoredRecommendations(ctx context.Context, q RecommendationPageQuery) (RecommendationPage, error) {
	if fallback, err := checkPageQuery(&q); fallback != "" || err != nil {
		return RecommendationPage{Fallback: fallback}, err
	}
	if err := s.checkAzurePricingMigration(ctx, q.Scope.Filter.RequireAzurePricingMigration); err != nil {
		return RecommendationPage{}, err
	}
	rules, err := pageRulesJSON(q.Rules)
	if err != nil {
		return RecommendationPage{}, fmt.Errorf("failed to encode override rules: %w", err)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return RecommendationPage{}, fmt.Errorf("failed to begin recommendations page transaction: %w", err)
	}
	defer rollbackTx(ctx, tx, "PageStoredRecommendations")
	if _, err = tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY"); err != nil {
		return RecommendationPage{}, fmt.Errorf("failed to set recommendations page isolation: %w", err)
	}
	page, err := pageInSnapshot(ctx, tx, &q, rules)
	if err != nil {
		return RecommendationPage{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return RecommendationPage{}, fmt.Errorf("failed to commit recommendations page transaction: %w", err)
	}
	return page, nil
}

// checkPageQuery rejects a malformed query and reports a fallback for a
// cursor Postgres cannot compare: text cannot hold NUL or invalid UTF-8, but
// a client-supplied cursor can, and the Go scan compares it byte-wise all
// the same.
func checkPageQuery(q *RecommendationPageQuery) (fallback string, err error) {
	switch {
	case q.Limit < 1:
		return "", errors.New("recommendations page limit must be positive")
	case len(q.SuppressionKeys) > 0 && q.DecideSuppressed == nil:
		return "", errors.New("recommendations page has suppression keys but no decider")
	case !utf8.ValidString(q.AfterID) || strings.ContainsRune(q.AfterID, 0):
		return "cursor is not valid UTF-8 text", nil
	}
	return "", nil
}

// pageInSnapshot is PageStoredRecommendations' reads inside its transaction.
func pageInSnapshot(ctx context.Context, tx pgx.Tx, q *RecommendationPageQuery, rules []byte) (RecommendationPage, error) {
	keepRowIDs, err := decideTouchedRows(ctx, tx, q)
	if err != nil {
		return RecommendationPage{}, err
	}
	page, err := queryPageAggregate(ctx, tx, q, rules, keepRowIDs)
	if err != nil || page.Fallback != "" {
		return page, err
	}
	query, args := buildPageRowsQuery(q, rules, keepRowIDs)
	rows, err := queryStoredRecommendations(ctx, tx, query, args)
	if err != nil {
		return RecommendationPage{}, err
	}
	page.Rows = make([]RecommendationRecord, 0, len(rows))
	for i := range rows {
		// The MinSavingsPct floor is checked again in Go on the page: a row
		// SQL kept but Go drops means the two disagree, and the request is
		// served by the Go scan rather than with a wrong row.
		if belowSavingsFloor(&rows[i].Rec, q.Scope.Filter.MinSavingsPct) {
			return RecommendationPage{Fallback: fmt.Sprintf("SQL kept recommendation %q below the savings floor", rows[i].Rec.ID)}, nil
		}
		page.Rows = append(page.Rows, rows[i].Rec)
	}
	return page, nil
}

// decideTouchedRows reads the rows on a suppression 6-tuple and returns the
// primary keys q.DecideSuppressed keeps; nil when there are no suppressions.
func decideTouchedRows(ctx context.Context, tx pgx.Tx, q *RecommendationPageQuery) ([]string, error) {
	if len(q.SuppressionKeys) == 0 {
		return nil, nil
	}
	query, args := buildPageTouchedQuery(q)
	touched, err := queryStoredRecommendations(ctx, tx, query, args)
	if err != nil {
		return nil, err
	}
	return q.DecideSuppressed(touched)
}

// belowSavingsFloor is decodeStoredRecommendation's MinSavingsPct check.
func belowSavingsFloor(rec *RecommendationRecord, floor float64) bool {
	if floor <= 0 {
		return false
	}
	pct, ok := recEffectiveSavingsPct(rec)
	return ok && pct < floor
}

// queryPageAggregate runs the summary query and reports a fallback when any
// in-scope row is unsure or two surviving rows share an ID.
func queryPageAggregate(ctx context.Context, tx pgx.Tx, q *RecommendationPageQuery, rules []byte, keepRowIDs []string) (RecommendationPage, error) {
	query, args := buildPageAggregateQuery(q, rules, keepRowIDs)
	var unsure, distinct int
	var page RecommendationPage
	err := tx.QueryRow(ctx, query, args...).Scan(&unsure, &page.TotalCount, &distinct, &page.TotalSavings, &page.TotalUpfront, &page.Regions)
	if err != nil {
		return RecommendationPage{}, fmt.Errorf("failed to aggregate recommendations: %w", err)
	}
	switch {
	case unsure > 0:
		return RecommendationPage{Fallback: fmt.Sprintf("%d rows need the Go filters", unsure)}, nil
	case distinct != page.TotalCount:
		return RecommendationPage{Fallback: "recommendation IDs are not unique"}, nil
	}
	return page, nil
}

// queryStoredRecommendations runs a query selecting (id::text, payload) and
// decodes each payload.
func queryStoredRecommendations(ctx context.Context, tx pgx.Tx, query string, args []any) ([]StoredRecommendation, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query recommendations page rows: %w", err)
	}
	defer rows.Close()
	var out []StoredRecommendation
	for rows.Next() {
		var row StoredRecommendation
		var payload []byte
		if err := rows.Scan(&row.RowID, &payload); err != nil {
			return nil, fmt.Errorf("failed to scan recommendations page row: %w", err)
		}
		if err := json.Unmarshal(payload, &row.Rec); err != nil {
			return nil, fmt.Errorf("failed to unmarshal payload: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recommendations page rows iteration: %w", err)
	}
	return out, nil
}
