-- Migration 000143 (issue #999): an operator's audited settlement of a failed
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
       AND (outcome_settled_at IS NULL) = (outcome_settlement IS NULL));
