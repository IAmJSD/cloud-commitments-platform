package config

import (
	"context"
	"encoding/json"
	"fmt"
)

const ladderTimelineQuery = `
WITH scoped AS MATERIALIZED (
 SELECT t.id, t.run_id, t.config_id, t.layer_type, t.term, t.payment_option,
        t.status, r.status AS run_status, t.execution_id, t.revision,
        t.amount_usd_hr::text AS amount_usd_hr, t.amount_basis, t.commitment_rate,
        t.scheduled_date, t.created_at
 FROM ladder_tranches t
 JOIN ladder_configs c ON c.id = t.config_id
 JOIN ladder_runs r ON r.id = t.run_id AND r.config_id = c.id
 JOIN cloud_accounts a ON a.id = c.cloud_account_id AND a.provider = c.provider
 WHERE c.cloud_account_id = $1 AND c.provider = $2
), page AS (
 SELECT * FROM scoped
 WHERE $3::timestamptz IS NULL OR (created_at, id) > ($3, $4::uuid)
 ORDER BY created_at, id LIMIT $5
)
SELECT COUNT(*), COALESCE(SUM(amount_usd_hr::numeric), 0::numeric(20,6))::text,
       COALESCE((SELECT jsonb_agg(to_jsonb(page) ORDER BY created_at, id) FROM page), '[]'::jsonb),
       COALESCE(jsonb_agg(jsonb_build_array(amount_usd_hr, amount_basis, commitment_rate)), '[]'::jsonb)
FROM scoped`

const ladderTimelineRunsQuery = `
WITH scoped AS MATERIALIZED (
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
), page AS (
 SELECT * FROM scoped
 WHERE $3::timestamptz IS NULL OR (created_at, id) > ($3, $4::uuid)
 ORDER BY created_at, id LIMIT $5
)
SELECT COUNT(*),
       COALESCE((SELECT jsonb_agg(to_jsonb(page) ORDER BY created_at, id) FROM page), '[]'::jsonb)
FROM scoped`

func ladderTimelineArgs(accountID, provider string, cursor *LadderTimelineCursor) []any {
	if cursor == nil {
		return []any{accountID, provider, nil, nil, LadderTimelinePageSize + 1}
	}
	return []any{accountID, provider, cursor.CreatedAt, cursor.ID, LadderTimelinePageSize + 1}
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
	if len(page.Events) > LadderTimelinePageSize {
		page.Events = page.Events[:LadderTimelinePageSize]
		last := page.Events[len(page.Events)-1]
		page.NextCursor = &LadderTimelineCursor{CreatedAt: last.CreatedAt, ID: last.ID}
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
	if len(page.Runs) > LadderTimelinePageSize {
		page.Runs = page.Runs[:LadderTimelinePageSize]
		last := page.Runs[len(page.Runs)-1]
		page.NextCursor = &LadderTimelineCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}
