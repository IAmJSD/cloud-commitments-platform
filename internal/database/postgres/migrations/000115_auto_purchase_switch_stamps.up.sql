-- Migration 000115 (platform#631 R1a): who last changed the auto-purchase
-- kill switch (global_config.auto_purchase_enabled), when, and with what kind
-- of credential. Nullable, additive, no backfill: NULL means "never changed
-- since the column existed". Nothing writes these until R1b. Idempotent.
ALTER TABLE global_config
    ADD COLUMN IF NOT EXISTS auto_purchase_changed_by TEXT,
    ADD COLUMN IF NOT EXISTS auto_purchase_changed_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS auto_purchase_changed_via TEXT;
