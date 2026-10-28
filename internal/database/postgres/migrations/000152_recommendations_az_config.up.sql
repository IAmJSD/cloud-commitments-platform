-- Migration 000152: az_config generated column and a 12-column unique index on
-- recommendations (#408, expand step of the RDS AZ-config identity work).
--
-- Cost Explorer reports Single-AZ and Multi-AZ RDS reservations of the same
-- instance class, engine, term and payment as separate recommendations. They
-- are different offerings with different prices, but the 11-column natural key
-- cannot tell them apart, so a batch carrying both fails the whole collection
-- write with SQLSTATE 21000. This migration adds the AZ configuration to the
-- schema; nothing uses it yet.
--
-- The column is derived verbatim from the stored payload (the JSON of the
-- RecommendationRecord) at payload->'details' (the DatabaseDetails blob), so
-- existing rows are backfilled by the table rewrite with no data migration.
-- No trimming or case-folding. Absent Details, JSON null, a missing key and a
-- null value all become ''. Every non-database row is ''.
--
-- Two indexes are added:
-- - recommendations_natural_key_az_tp_azc_idx: the 11 columns plus az_config,
--   the new upsert arbiter.
-- - recommendations_natural_key_source_azc_idx: the source-account key of
--   migration 000120 (#799) plus az_config. Every unique index without
--   az_config rejects a Single-AZ/Multi-AZ pair, so the source-aware key has
--   to gain the column too; 000153 retires the 000120 index it replaces.
--
-- The existing 11-column recommendations_natural_key_az_tp_idx and the
-- 000120 recommendations_natural_key_source_idx are KEPT, so the current
-- ON CONFLICT on the 11 columns keeps working for old and new code. Two rows
-- that differ only by az_config are still rejected by those indexes until
-- migration 000153 drops them (applied only once the store upserts on the
-- 12-column index).
--
-- Lock impact: ADD COLUMN ... GENERATED ... STORED rewrites the table and the
-- index build blocks writes. The table is a regenerable cache re-populated
-- every collection cycle, so this is acceptable. CREATE INDEX CONCURRENTLY is
-- not possible: the migration runner wraps every migration in a transaction.
ALTER TABLE recommendations
    ADD COLUMN IF NOT EXISTS az_config TEXT
    GENERATED ALWAYS AS (COALESCE(payload->'details'->>'az_config', '')) STORED;

CREATE UNIQUE INDEX IF NOT EXISTS recommendations_natural_key_az_tp_azc_idx
    ON recommendations (account_key, provider, service, region, resource_type, engine, term, payment_option, availability_zone, platform, tenancy, az_config);

CREATE UNIQUE INDEX IF NOT EXISTS recommendations_natural_key_source_azc_idx
    ON recommendations (account_key, provider, service, region, resource_type, engine, term, payment_option, availability_zone, platform, tenancy, az_config, source_account_key);
