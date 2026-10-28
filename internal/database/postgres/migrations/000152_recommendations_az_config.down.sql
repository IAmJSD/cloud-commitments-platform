DROP INDEX IF EXISTS recommendations_natural_key_source_azc_idx;
DROP INDEX IF EXISTS recommendations_natural_key_az_tp_azc_idx;

ALTER TABLE recommendations DROP COLUMN IF EXISTS az_config;
