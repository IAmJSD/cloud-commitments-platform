-- Migration 000155 (issue #999): an operator's audited settlement of a failed
-- execution whose outcome is unknown (failed without no_submission proof).
--
-- Settling never changes status and never makes the row count as unbought:
-- the ramp ledger (holdsCapacity, ramp_ledger_holds) and the retention trigger
-- still treat it as possibly bought. It only lets a managed plan's delete
-- detach the row, and it stops the row from being retried (a retry of a row
-- detached from its plan would buy with no plan).
--
-- outcome_settled_by is the acting principal as text (a users.id, or the admin
-- API key sentinel), so the audit survives the user being deleted.
-- outcome_settlement is the complete, fixed-format record the store wrote.
ALTER TABLE purchase_executions
    ADD COLUMN IF NOT EXISTS outcome_settled_at TIMESTAMPTZ NULL,
    ADD COLUMN IF NOT EXISTS outcome_settled_by TEXT NULL,
    ADD COLUMN IF NOT EXISTS outcome_settlement TEXT NULL;

ALTER TABLE purchase_executions
    ADD CONSTRAINT purchase_executions_outcome_settlement_complete
    CHECK ((outcome_settled_at IS NULL) = (outcome_settled_by IS NULL)
       AND (outcome_settled_at IS NULL) = (outcome_settlement IS NULL))
    NOT VALID;
-- NOT VALID still checks every new write and skips the table scan under the
-- ACCESS EXCLUSIVE lock; 000156 validates it.

-- A settled row is never retried. LinkRetryExecutionAtomic enforces it, and
-- this trigger holds it for any other writer, such as a binary from before
-- this migration during a rollout or rollback.
CREATE OR REPLACE FUNCTION purchase_executions_refuse_retry_of_settled()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.retry_execution_id IS NULL AND NEW.retry_execution_id IS NOT NULL
       AND (OLD.outcome_settled_at IS NOT NULL OR NEW.outcome_settled_at IS NOT NULL) THEN
        RAISE EXCEPTION 'execution % is settled and cannot be retried', OLD.execution_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS purchase_executions_refuse_retry_of_settled ON purchase_executions;
CREATE TRIGGER purchase_executions_refuse_retry_of_settled
    BEFORE UPDATE OF retry_execution_id ON purchase_executions
    FOR EACH ROW EXECUTE FUNCTION purchase_executions_refuse_retry_of_settled();
