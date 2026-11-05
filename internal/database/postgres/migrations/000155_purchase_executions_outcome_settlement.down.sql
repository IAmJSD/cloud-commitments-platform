-- Reverse 000155 (issue #999). Refuses while any settlement exists: dropping
-- the columns would let a settled row be retried and block its plan's delete
-- again. The lock and the check share one DO block, so no settlement can land
-- between them.
DO $$
BEGIN
    LOCK TABLE purchase_executions IN ACCESS EXCLUSIVE MODE;
    IF EXISTS (SELECT 1 FROM purchase_executions WHERE outcome_settled_at IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot reverse 000155: purchase_executions has operator settlements';
    END IF;
    DROP TRIGGER IF EXISTS purchase_executions_refuse_retry_of_settled ON purchase_executions;
    DROP FUNCTION IF EXISTS purchase_executions_refuse_retry_of_settled();
    ALTER TABLE purchase_executions
        DROP CONSTRAINT IF EXISTS purchase_executions_outcome_settlement_complete;
    ALTER TABLE purchase_executions
        DROP COLUMN IF EXISTS outcome_settlement,
        DROP COLUMN IF EXISTS outcome_settled_by,
        DROP COLUMN IF EXISTS outcome_settled_at;
END
$$;
