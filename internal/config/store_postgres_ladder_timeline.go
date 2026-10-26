package config

import (
	"context"
	"encoding/json"
	"fmt"
)

// The timeline reads return the whole account scope from ONE statement, so
// the counts, totals and rows all come from a single snapshot: a concurrent
// amendment, run or tranche insert lands either wholly before or wholly after
// the read, never between two pages of it (#668). Every page of the earlier
// paginated read re-aggregated the full scope, so loading n rows cost about
// n/100 statements each scanning n rows; this is one statement scanning n.
//
// $3/$4 is an optional (created_at, id) lower bound kept for a client that
// still follows a next_cursor from before this change: it narrows the rows
// returned, never the totals, and no next cursor is ever issued.
const ladderTimelineQuery = `
WITH scoped AS (
 SELECT t.id, t.run_id, t.config_id, t.layer_type, t.term, t.payment_option,
        t.status, r.status AS run_status, t.execution_id, t.revision,
        t.amount_usd_hr::text AS amount_usd_hr, t.amount_basis, t.commitment_rate,
        t.scheduled_date, t.created_at
 FROM ladder_tranches t
 JOIN ladder_configs c ON c.id = t.config_id
 JOIN ladder_runs r ON r.id = t.run_id AND r.config_id = c.id
 JOIN cloud_accounts a ON a.id = c.cloud_account_id AND a.provider = c.provider
 WHERE c.cloud_account_id = $1 AND c.provider = $2
)
SELECT COUNT(*), COALESCE(SUM(amount_usd_hr::numeric), 0::numeric(20,6))::text,
       COALESCE(jsonb_agg(to_jsonb(s) ORDER BY created_at, id)
         FILTER (WHERE $3::timestamptz IS NULL OR (created_at, id) > ($3, $4::uuid)), '[]'::jsonb),
       COALESCE(jsonb_agg(jsonb_build_array(amount_usd_hr, amount_basis, commitment_rate)), '[]'::jsonb)
FROM scoped s`

const ladderTimelineRunsQuery = `
WITH scoped AS (
 SELECT r.id, r.config_id, r.started_at, r.created_at, r.status,
        r.baseline_usd_hr::text AS baseline_usd_hr, r.target_usd_hr::text AS target_usd_hr,
        r.existing_usd_hr::text AS existing_usd_hr, r.gap_usd_hr::text AS gap_usd_hr,
        r.total_hourly_commit::text AS total_hourly_commit,
        COALESCE((SELECT jsonb_agg(jsonb_build_object(
          'action', action->'action', 'layer', action->'layer',
          'rationale', action->'rationale', 'data_sources', action->'data_sources'))
          FROM jsonb_array_elements(COALESCE(r.plan->'actions', '[]'::jsonb)) action),
          '[]'::jsonb) AS actions
 FROM ladder_runs r
 JOIN ladder_configs c ON c.id = r.config_id
 JOIN cloud_accounts a ON a.id = c.cloud_account_id AND a.provider = c.provider
 WHERE c.cloud_account_id = $1 AND c.provider = $2
)
SELECT COUNT(*),
       COALESCE(jsonb_agg(to_jsonb(s) ORDER BY created_at, id)
         FILTER (WHERE $3::timestamptz IS NULL OR (created_at, id) > ($3, $4::uuid)), '[]'::jsonb)
FROM scoped s`

func ladderTimelineArgs(accountID, provider string, cursor *LadderTimelineCursor) []any {
	if cursor == nil {
		return []any{accountID, provider, nil, nil}
	}
	return []any{accountID, provider, cursor.CreatedAt, cursor.ID}
}

func (s *PostgresStore) ListLadderTimeline(ctx context.Context, accountID, provider string, cursor *LadderTimelineCursor) (*LadderTimelinePage, error) {
	page := &LadderTimelinePage{}
	var raw, scopeRaw []byte
	if err := s.db.QueryRow(ctx, ladderTimelineQuery, ladderTimelineArgs(accountID, provider, cursor)...).
		Scan(&page.TotalCount, &page.TotalUSDHr, &raw, &scopeRaw); err != nil {
		return nil, fmt.Errorf("list ladder timeline: %w", err)
	}
	if err := json.Unmarshal(raw, &page.Events); err != nil {
		return nil, fmt.Errorf("decode ladder timeline: %w", err)
	}
	if err := fillLadderTimelineOnDemand(page, scopeRaw); err != nil {
		return nil, err
	}
	return page, nil
}

func (s *PostgresStore) ListLadderTimelineRuns(ctx context.Context, accountID, provider string, cursor *LadderTimelineCursor) (*LadderTimelineRunPage, error) {
	page := &LadderTimelineRunPage{}
	var raw []byte
	if err := s.db.QueryRow(ctx, ladderTimelineRunsQuery, ladderTimelineArgs(accountID, provider, cursor)...).
		Scan(&page.TotalCount, &raw); err != nil {
		return nil, fmt.Errorf("list ladder runs: %w", err)
	}
	if err := json.Unmarshal(raw, &page.Runs); err != nil {
		return nil, fmt.Errorf("decode ladder runs: %w", err)
	}
	return page, nil
}
