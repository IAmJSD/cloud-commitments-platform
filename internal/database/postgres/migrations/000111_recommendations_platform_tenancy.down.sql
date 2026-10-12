DROP INDEX IF EXISTS recommendations_natural_key_az_tp_idx;

ALTER TABLE recommendations
    DROP COLUMN IF EXISTS platform,
    DROP COLUMN IF EXISTS tenancy;
