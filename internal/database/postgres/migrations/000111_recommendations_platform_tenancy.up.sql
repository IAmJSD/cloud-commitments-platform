-- Migration 000111: platform and tenancy generated columns and an 11-column
-- unique index on recommendations (#806, PR-A of the EC2 platform/tenancy work).
--
-- Two EC2 recommendations for the same instance type, term, payment and AZ can
-- differ only by platform (e.g. "Linux/UNIX" vs "Windows") or tenancy
-- ("default" vs "dedicated"), but the 9-column natural key cannot tell them
-- apart. This migration adds both to the schema; nothing uses them yet.
--
-- Both columns are derived verbatim from the stored payload (the JSON of the
-- RecommendationRecord) at payload->'details' (the ComputeDetails blob), so
-- existing rows are backfilled by the table rewrite with no data migration.
-- No trimming or case-folding: AWS platform is the raw Cost Explorer string.
-- Absent Details, JSON null, a missing key and a null value all become ''.
--
-- The existing 9-column recommendations_natural_key_az_idx is KEPT, so the
-- current ON CONFLICT (..., availability_zone) upsert keeps working for old and
-- new code. Two rows that differ only by platform or tenancy are still rejected
-- by that index until a later migration drops it (PR-E, released together with
-- the store change to the 11-column conflict target).
--
-- Both columns are added in ONE ALTER so the table is rewritten once.
-- Lock impact: ADD COLUMN ... GENERATED ... STORED rewrites the table and the
-- index build blocks writes. The table is a regenerable cache re-populated
-- every collection cycle, so this is acceptable. CREATE INDEX CONCURRENTLY is
-- not used: the runner adds no BEGIN/COMMIT, but it sends this two-statement
-- file as one simple query, which PostgreSQL runs as one implicit transaction
-- where CONCURRENTLY is rejected, and TestMigrationsAreTransactional keeps it
-- out of migrations as policy.
ALTER TABLE recommendations
    ADD COLUMN IF NOT EXISTS platform TEXT
    GENERATED ALWAYS AS (COALESCE(payload->'details'->>'platform', '')) STORED,
    ADD COLUMN IF NOT EXISTS tenancy TEXT
    GENERATED ALWAYS AS (COALESCE(payload->'details'->>'tenancy', '')) STORED;

CREATE UNIQUE INDEX IF NOT EXISTS recommendations_natural_key_az_tp_idx
    ON recommendations (account_key, provider, service, region, resource_type, engine, term, payment_option, availability_zone, platform, tenancy);
