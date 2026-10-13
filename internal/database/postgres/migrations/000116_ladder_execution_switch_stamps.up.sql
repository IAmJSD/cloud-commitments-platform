-- Migration 000116 (platform#118 PR2): who last changed the ladder execution
-- switch (global_config.ladder_execution_enabled), when, and with what kind of
-- credential. Nullable, additive, no backfill: NULL means "never changed since
-- the column existed". Idempotent.
ALTER TABLE global_config
    ADD COLUMN IF NOT EXISTS ladder_execution_changed_by TEXT,
    ADD COLUMN IF NOT EXISTS ladder_execution_changed_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS ladder_execution_changed_via TEXT;
