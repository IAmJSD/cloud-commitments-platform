package config

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// HistorySource identifies the table a row of the History view's paged stream
// comes from. Its value is part of the stream order (see HistoryPageKey), so
// the constants must keep their relative order.
type HistorySource int

const (
	// HistorySourceExecution is a terminal purchase_executions row (failed,
	// expired, canceled, audit-gap completed, partially completed).
	HistorySourceExecution HistorySource = 1
	// HistorySourcePurchase is a purchase_history row.
	HistorySourcePurchase HistorySource = 2
)

// HistoryPageKey is a position in the History view's paged stream (issue
// #674): purchase_history rows and terminal executions merged in one order,
// timestamp DESC, then Source DESC, then ID DESC. Timestamp is
// purchase_history.timestamp or purchase_executions.scheduled_date, and ID is
// purchase_history.id or purchase_executions.execution_id (both UUIDs, unique
// per table), so the key is unique across the stream and a keyset page never
// repeats or skips a row.
type HistoryPageKey struct {
	Timestamp time.Time
	Source    HistorySource
	ID        string
}

// SortsBefore reports whether k comes before o in the stream order. IDs are
// compared as canonical lowercase UUID strings, which order the same way as
// PostgreSQL's uuid type.
func (k HistoryPageKey) SortsBefore(o HistoryPageKey) bool {
	if !k.Timestamp.Equal(o.Timestamp) {
		return k.Timestamp.After(o.Timestamp)
	}
	if k.Source != o.Source {
		return k.Source > o.Source
	}
	return k.ID > o.ID
}

// PurchaseHistoryPageRow is a purchase_history row read by
// ListPurchaseHistoryPage, with the row's primary key: the keyset tiebreaker
// for rows that share a timestamp, which purchase_id cannot be because legacy
// rows may repeat it (migration 000108).
type PurchaseHistoryPageRow struct {
	PurchaseHistoryRecord
	RowID string
}

// Key returns the row's position in the History stream.
func (r *PurchaseHistoryPageRow) Key() HistoryPageKey {
	return HistoryPageKey{Timestamp: r.Timestamp, Source: HistorySourcePurchase, ID: r.RowID}
}

// HistoryExecutionsPageQuery selects one keyset page of executions for the
// History stream.
type HistoryExecutionsPageQuery struct {
	// Statuses the rows must have. Clean completed rows (status completed,
	// empty error) are always excluded, as in GetExecutionsByStatuses.
	Statuses []string
	// PlanID matches purchase_executions.plan_id exactly; empty skips the clause.
	PlanID string
	// Start and End bound scheduled_date, inclusive; nil leaves a side open.
	Start *time.Time
	End   *time.Time
	// After is the stream position to continue from; nil starts at the top.
	After *HistoryPageKey
	// Limit caps the row count; clamped to [1, MaxListLimit] with a
	// DefaultListLimit fallback when <= 0.
	Limit int
}

// clampHistoryPageLimit applies the list limit bounds every History page
// query shares.
func clampHistoryPageLimit(limit int) int {
	if limit <= 0 {
		return DefaultListLimit
	}
	return min(limit, MaxListLimit)
}

// historyKeysetCondition returns the predicate selecting the rows of source
// src that come after `after` in the stream order, and the bind args with its
// parameters appended. At an equal timestamp a lower source sorts later, so
// when src differs from the cursor's source the timestamp comparison alone
// decides; within the cursor's own source the ID breaks the tie.
func historyKeysetCondition(tsCol, idCol string, src HistorySource, after *HistoryPageKey, args []any) (cond string, outArgs []any) {
	args = append(args, after.Timestamp)
	ts := len(args)
	switch {
	case src < after.Source:
		return fmt.Sprintf("%s <= $%d", tsCol, ts), args
	case src > after.Source:
		return fmt.Sprintf("%s < $%d", tsCol, ts), args
	}
	args = append(args, after.ID)
	return fmt.Sprintf("(%s < $%d OR (%s = $%d AND %s < $%d::uuid))", tsCol, ts, tsCol, ts, idCol, len(args)), args
}

// whereClause joins conditions into a WHERE clause with a leading space, or
// "" when there are none.
func whereClause(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

// andWhere adds cond to a WHERE clause built by whereClause or
// purchaseHistoryFilterWhere ("" or " WHERE ...").
func andWhere(where, cond string) string {
	if where == "" {
		return " WHERE " + cond
	}
	return where + " AND " + cond
}

// ListPurchaseHistoryPage returns up to filter.Limit purchase_history rows
// matching filter, in History stream order (timestamp DESC, id DESC),
// starting after `after` (nil starts at the newest row). See
// StoreInterface.ListPurchaseHistoryPage.
func (s *PostgresStore) ListPurchaseHistoryPage(ctx context.Context, filter PurchaseHistoryFilter, after *HistoryPageKey) ([]PurchaseHistoryPageRow, error) {
	// The filter predicate is purchaseHistoryFilterWhere, the one builder
	// GetPurchaseHistoryFiltered and HistorySummaryAggregate share, so a page
	// lists exactly the rows the summary counts (issue #675).
	where, args := purchaseHistoryFilterWhere(filter)
	if after != nil {
		var cond string
		cond, args = historyKeysetCondition("timestamp", "id", HistorySourcePurchase, after, args)
		where = andWhere(where, cond)
	}
	args = append(args, clampHistoryPageLimit(filter.Limit))
	query := fmt.Sprintf(`
		SELECT id::text, account_id, purchase_id, timestamp, provider, service, region,
		       resource_type, count, term, payment, upfront_cost, monthly_cost,
		       estimated_savings, plan_id, plan_name, ramp_step, cloud_account_id,
		       revocation_window_closes_at, revoked_at, revoked_via, support_case_id,
		       offering_class, listing_id, listing_state
		FROM purchase_history%s
		ORDER BY timestamp DESC, id DESC
		LIMIT $%d
	`, where, len(args))

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query purchase history page: %w", err)
	}
	defer rows.Close()

	out := make([]PurchaseHistoryPageRow, 0)
	for rows.Next() {
		var row PurchaseHistoryPageRow
		record, err := scanPurchaseHistoryRow(rowIDPrefixedRows{Rows: rows, id: &row.RowID})
		if err != nil {
			return nil, err
		}
		row.PurchaseHistoryRecord = record
		out = append(out, row)
	}
	return out, rows.Err()
}

// rowIDPrefixedRows lets scanPurchaseHistoryRow, the one scanner for the
// purchase_history column list, read a result set whose first column is the
// row's id: Scan reads that column into id and hands the rest to the caller's
// destinations.
type rowIDPrefixedRows struct {
	pgx.Rows
	id *string
}

func (r rowIDPrefixedRows) Scan(dest ...any) error {
	return r.Rows.Scan(append([]any{r.id}, dest...)...)
}

// ListHistoryExecutionsPage returns up to q.Limit executions in q.Statuses,
// clean completed rows excluded, in History stream order (scheduled_date DESC,
// execution_id DESC), starting after q.After. See
// StoreInterface.ListHistoryExecutionsPage.
func (s *PostgresStore) ListHistoryExecutionsPage(ctx context.Context, q HistoryExecutionsPageQuery) ([]PurchaseExecution, error) {
	if len(q.Statuses) == 0 {
		return nil, nil
	}
	args := []any{q.Statuses}
	// historyExecutionsPredicate is shared with GetExecutionsByStatuses and
	// HistorySummaryAggregate, so the stream and the summary select the same
	// executions (issue #675).
	conds := []string{historyExecutionsPredicate}
	if q.PlanID != "" {
		args = append(args, q.PlanID)
		conds = append(conds, fmt.Sprintf("plan_id = $%d", len(args)))
	}
	if q.Start != nil {
		args = append(args, *q.Start)
		conds = append(conds, fmt.Sprintf("scheduled_date >= $%d", len(args)))
	}
	if q.End != nil {
		args = append(args, *q.End)
		conds = append(conds, fmt.Sprintf("scheduled_date <= $%d", len(args)))
	}
	if q.After != nil {
		var cond string
		cond, args = historyKeysetCondition("scheduled_date", "execution_id", HistorySourceExecution, q.After, args)
		conds = append(conds, cond)
	}
	args = append(args, clampHistoryPageLimit(q.Limit))
	// cancelled_by is the legacy column name (migration 000035, rename tracked in #1277).
	query := fmt.Sprintf(`
		SELECT plan_id, execution_id, status, step_number, scheduled_date,
		       notification_sent, approval_token_hash, recommendations,
		       total_upfront_cost, estimated_savings, completed_at, error, expires_at,
		       cloud_account_id, source, approved_by, COALESCE(canceled_by, cancelled_by) AS cancelled_by, capacity_percent,
		       created_by_user_id, retry_execution_id, retry_attempt_n,
		       approval_token_expires_at,
		       executed_by_user_id, executed_at, pre_approval_skip_reason,
		       idempotency_key, scheduled_execution_at, no_submission, created_by_system
		FROM purchase_executions%s
		ORDER BY scheduled_date DESC, execution_id DESC
		LIMIT $%d
	`, whereClause(conds), len(args))
	return s.queryExecutions(ctx, query, args...)
}
