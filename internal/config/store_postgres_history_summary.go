package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// historyExecutionsPredicate is the purchase_executions predicate shared by
// GetExecutionsByStatuses (the History row fetch) and HistorySummaryAggregate
// (the History summary totals), with the status list bound as $1. Clean
// completed executions are excluded because their purchase_history rows
// already represent them; a completed execution with an error (the audit-gap
// case) is kept.
const historyExecutionsPredicate = `status = ANY($1)
		  AND NOT (status = 'completed' AND COALESCE(error, '') = '')`

// HistorySummaryFilter selects the rows HistorySummaryAggregate counts.
type HistorySummaryFilter struct {
	// History selects purchase_history rows with exactly the predicate
	// GetPurchaseHistoryFiltered applies (purchaseHistoryFilterWhere). Limit is
	// ignored: the aggregate covers every matching row.
	History PurchaseHistoryFilter
	// ExecutionStatuses selects purchase_executions rows with exactly the
	// predicate GetExecutionsByStatuses applies (historyExecutionsPredicate).
	// Empty skips the executions half.
	ExecutionStatuses []string
	// ExecutionStart/ExecutionEnd bound purchase_executions.scheduled_date,
	// inclusive on both sides like the History handler's in-memory date check.
	// nil leaves that side open.
	ExecutionStart *time.Time
	ExecutionEnd   *time.Time
}

// HistorySummaryGroups is the result of HistorySummaryAggregate: row counts and
// dollar sums grouped by the columns the History handler's provider, account
// and account-scope checks read. The caller applies those checks to each group
// with the same functions it applies to fetched rows, so the scope predicate
// cannot drift between the row list and the totals. The number of groups is
// bounded by the distinct account/provider/status combinations, not by row
// volume.
type HistorySummaryGroups struct {
	Purchases  []PurchaseHistorySummaryGroup
	Executions []ExecutionSummaryGroup
}

// PurchaseHistorySummaryGroup aggregates the purchase_history rows that share
// account_id, cloud_account_id, provider and revoked state.
type PurchaseHistorySummaryGroup struct {
	AccountID      string
	CloudAccountID *string
	Provider       string
	Revoked        bool
	Count          int
	// UpfrontCost is the sum of upfront_cost, or nil when any row in the
	// group has an unknown (NULL) upfront cost, matching how the History
	// summary treats a single unknown cost.
	UpfrontCost      *float64
	EstimatedSavings float64
}

// ExecutionSummaryGroup aggregates the purchase_executions rows that share
// status, cloud_account_id, created_by_user_id and the ordered list of their
// recommendations' (provider, cloud_account_id) pairs. Recommendations carries
// only Provider and CloudAccountID (nil when absent or empty), in stored
// order, which is everything the History handler's execution filters and
// scope check read.
type ExecutionSummaryGroup struct {
	Status          string
	CloudAccountID  *string
	CreatedByUserID *string
	Recommendations []RecommendationRecord
	Count           int
}

// purchaseHistorySummaryQuery groups purchase_history rows; %s is the WHERE
// clause from purchaseHistoryFilterWhere.
const purchaseHistorySummaryQuery = `
		SELECT account_id, cloud_account_id, provider, (revoked_at IS NOT NULL) AS revoked,
		       COUNT(*),
		       CASE WHEN COUNT(upfront_cost) = COUNT(*) THEN SUM(upfront_cost) END,
		       COALESCE(SUM(estimated_savings), 0)
		FROM purchase_history%s
		GROUP BY account_id, cloud_account_id, provider, (revoked_at IS NOT NULL)
	`

// executionSummaryRecsKey projects an execution's recommendations to the
// ordered [provider, cloud_account_id] pairs the History handler reads. A
// non-array value (json.Marshal of a nil slice stores 'null') projects to an
// empty list, as encoding/json decodes it to an empty slice. Keys are the
// lowercase JSON tags json.Marshal writes for RecommendationRecord.
const executionSummaryRecsKey = `CASE WHEN jsonb_typeof(recommendations) = 'array' THEN
		         (SELECT COALESCE(jsonb_agg(jsonb_build_array(
		                     COALESCE(r.value->>'provider', ''),
		                     COALESCE(r.value->>'cloud_account_id', '')) ORDER BY r.ordinality), '[]'::jsonb)
		            FROM jsonb_array_elements(recommendations) WITH ORDINALITY AS r)
		       ELSE '[]'::jsonb END`

// executionSummaryQuery builds the grouped purchase_executions query and its
// bind args.
func executionSummaryQuery(filter HistorySummaryFilter) (query string, args []any) {
	where := historyExecutionsPredicate
	args = []any{filter.ExecutionStatuses}
	if filter.History.PlanID != "" {
		args = append(args, filter.History.PlanID)
		where += fmt.Sprintf(" AND plan_id = $%d", len(args))
	}
	if filter.ExecutionStart != nil {
		args = append(args, *filter.ExecutionStart)
		where += fmt.Sprintf(" AND scheduled_date >= $%d", len(args))
	}
	if filter.ExecutionEnd != nil {
		args = append(args, *filter.ExecutionEnd)
		where += fmt.Sprintf(" AND scheduled_date <= $%d", len(args))
	}
	query = fmt.Sprintf(`
		SELECT status, cloud_account_id, created_by_user_id, %s AS recs_key, COUNT(*)
		FROM purchase_executions
		WHERE %s
		GROUP BY status, cloud_account_id, created_by_user_id, recs_key
	`, executionSummaryRecsKey, where)
	return query, args
}

// HistorySummaryAggregate returns grouped counts and dollar sums over every
// purchase_history and purchase_executions row the History view's fetches
// would select without their row caps (issue #675). Both halves are read in
// one REPEATABLE READ, READ ONLY transaction, so a purchase that moves from an
// in-flight execution to a purchase_history row while the totals are computed
// is counted exactly once.
func (s *PostgresStore) HistorySummaryAggregate(ctx context.Context, filter HistorySummaryFilter) (*HistorySummaryGroups, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to begin history summary transaction: %w", err)
	}
	defer rollbackTx(ctx, tx, "HistorySummaryAggregate")
	if _, err = tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY"); err != nil {
		return nil, fmt.Errorf("failed to set history summary isolation: %w", err)
	}
	out := &HistorySummaryGroups{}
	if out.Purchases, err = queryPurchaseHistorySummary(ctx, tx, filter.History); err != nil {
		return nil, err
	}
	if len(filter.ExecutionStatuses) > 0 {
		if out.Executions, err = queryExecutionSummary(ctx, tx, filter); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("failed to commit history summary transaction: %w", err)
	}
	return out, nil
}

func queryPurchaseHistorySummary(ctx context.Context, tx pgx.Tx, filter PurchaseHistoryFilter) ([]PurchaseHistorySummaryGroup, error) {
	where, args := purchaseHistoryFilterWhere(filter)
	rows, err := tx.Query(ctx, fmt.Sprintf(purchaseHistorySummaryQuery, where), args...)
	if err != nil {
		return nil, fmt.Errorf("failed to aggregate purchase history: %w", err)
	}
	defer rows.Close()
	groups := make([]PurchaseHistorySummaryGroup, 0)
	for rows.Next() {
		var g PurchaseHistorySummaryGroup
		var cloudAccountID sql.NullString
		var upfront sql.NullFloat64
		if err := rows.Scan(&g.AccountID, &cloudAccountID, &g.Provider, &g.Revoked, &g.Count, &upfront, &g.EstimatedSavings); err != nil {
			return nil, fmt.Errorf("failed to scan purchase history aggregate: %w", err)
		}
		if cloudAccountID.Valid {
			g.CloudAccountID = &cloudAccountID.String
		}
		g.UpfrontCost = nullPtrFromNullFloat64(upfront)
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

func queryExecutionSummary(ctx context.Context, tx pgx.Tx, filter HistorySummaryFilter) ([]ExecutionSummaryGroup, error) {
	query, args := executionSummaryQuery(filter)
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to aggregate executions: %w", err)
	}
	defer rows.Close()
	groups := make([]ExecutionSummaryGroup, 0)
	for rows.Next() {
		var g ExecutionSummaryGroup
		var recsKey []byte
		if err = rows.Scan(&g.Status, &g.CloudAccountID, &g.CreatedByUserID, &recsKey, &g.Count); err != nil {
			return nil, fmt.Errorf("failed to scan execution aggregate: %w", err)
		}
		if g.Recommendations, err = decodeExecutionSummaryRecs(recsKey); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// decodeExecutionSummaryRecs turns the [[provider, cloud_account_id], ...]
// projection back into RecommendationRecords. An empty account id becomes a
// nil CloudAccountID; the History handler treats nil and "" alike.
func decodeExecutionSummaryRecs(raw []byte) ([]RecommendationRecord, error) {
	var pairs [][2]string
	if err := json.Unmarshal(raw, &pairs); err != nil {
		return nil, fmt.Errorf("failed to decode execution aggregate recommendations: %w", err)
	}
	recs := make([]RecommendationRecord, len(pairs))
	for i, p := range pairs {
		recs[i].Provider = p[0]
		if p[1] != "" {
			id := p[1]
			recs[i].CloudAccountID = &id
		}
	}
	return recs, nil
}
