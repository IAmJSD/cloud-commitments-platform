ALTER TABLE global_config
    DROP COLUMN IF EXISTS ladder_execution_changed_by,
    DROP COLUMN IF EXISTS ladder_execution_changed_at,
    DROP COLUMN IF EXISTS ladder_execution_changed_via;
