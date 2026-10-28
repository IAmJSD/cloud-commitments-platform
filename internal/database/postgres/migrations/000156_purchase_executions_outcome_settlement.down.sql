-- Reverse 000156 (issue #999). Lossy: operator settlements are dropped, so a
-- settled failed row blocks its managed plan's delete again.
ALTER TABLE purchase_executions
    DROP CONSTRAINT IF EXISTS purchase_executions_outcome_settlement_complete;
ALTER TABLE purchase_executions
    DROP COLUMN IF EXISTS outcome_settlement,
    DROP COLUMN IF EXISTS outcome_settled_by,
    DROP COLUMN IF EXISTS outcome_settled_at;
