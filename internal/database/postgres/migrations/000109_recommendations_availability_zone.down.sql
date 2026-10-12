DROP INDEX IF EXISTS recommendations_natural_key_az_idx;

ALTER TABLE recommendations
    DROP COLUMN IF EXISTS availability_zone;
