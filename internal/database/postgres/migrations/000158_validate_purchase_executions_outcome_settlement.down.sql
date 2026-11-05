-- A validated constraint cannot be un-validated; re-add it NOT VALID, as the
-- state 000157 leaves (same pattern as 000122).
ALTER TABLE purchase_executions
    DROP CONSTRAINT purchase_executions_outcome_settlement_complete;
ALTER TABLE purchase_executions
    ADD CONSTRAINT purchase_executions_outcome_settlement_complete
    CHECK ((outcome_settled_at IS NULL) = (outcome_settled_by IS NULL)
       AND (outcome_settled_at IS NULL) = (outcome_settlement IS NULL))
    NOT VALID;
