-- Migration 000109: availability_zone generated column and a 9-column unique
-- index on recommendations (#792, PR2a of the zonal EC2 AZ work).
--
-- Zonal EC2 recommendations for the same instance type, term and payment
-- differ only by availability zone, but the 8-column natural key cannot tell
-- them apart. This migration adds the AZ to the schema; nothing uses it yet.
--
-- The column is derived from the stored payload (the JSON of the
-- RecommendationRecord) so existing rows are backfilled by the table rewrite
-- with no data migration: the top-level "availability_zone" field wins, and
-- rows written before that field existed fall back to details.availability_zone
-- (the ComputeDetails blob). Anything else, including every non-zonal row,
-- becomes ''.
--
-- The existing 8-column recommendations_natural_key_idx is KEPT, so the
-- current ON CONFLICT (account_key, provider, service, region, resource_type,
-- engine, term, payment_option) upsert keeps working for old and new code. As a
-- consequence two zonal rows that differ only by AZ are still rejected by that
-- index until a follow-up migration replaces it (PR2c, together with the store
-- change to the 9-column conflict target).
--
-- Lock impact: ADD COLUMN ... GENERATED ... STORED rewrites the table and the
-- index build blocks writes, both under ACCESS EXCLUSIVE / SHARE locks for the
-- duration. The table is a regenerable cache re-populated every collection
-- cycle, so this is acceptable. CREATE INDEX CONCURRENTLY is not possible:
-- the migration runner wraps every migration in a transaction.
ALTER TABLE recommendations
    ADD COLUMN IF NOT EXISTS availability_zone TEXT
    GENERATED ALWAYS AS (
        COALESCE(payload->>'availability_zone', payload->'details'->>'availability_zone', '')
    ) STORED;

CREATE UNIQUE INDEX IF NOT EXISTS recommendations_natural_key_az_idx
    ON recommendations (account_key, provider, service, region, resource_type, engine, term, payment_option, availability_zone);
