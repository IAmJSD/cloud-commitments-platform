-- Validate the CHECK that 000155 added NOT VALID (issue #999); same split as
-- 000121/000122. VALIDATE scans without blocking writes.
ALTER TABLE purchase_executions
    VALIDATE CONSTRAINT purchase_executions_outcome_settlement_complete;
