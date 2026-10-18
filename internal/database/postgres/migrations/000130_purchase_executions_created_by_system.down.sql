-- Reverse 000130: drop the system-created marker (issue #233). Lossy: rows the
-- scheduler minted become indistinguishable from other NULL-creator rows.
ALTER TABLE purchase_executions
    DROP CONSTRAINT IF EXISTS purchase_executions_created_by_system_no_creator;
ALTER TABLE purchase_executions
    DROP COLUMN IF EXISTS created_by_system;
