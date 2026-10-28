-- Revert migration 000146: both columns become nullable again with no default,
-- as 000001 left them. The backfilled 0 / '' values stay: they are what the
-- Go readers already return for NULL, so there is nothing to restore.
ALTER TABLE purchase_history
    ALTER COLUMN ramp_step DROP NOT NULL,
    ALTER COLUMN ramp_step DROP DEFAULT;

ALTER TABLE purchase_executions
    ALTER COLUMN error DROP NOT NULL,
    ALTER COLUMN error DROP DEFAULT;
