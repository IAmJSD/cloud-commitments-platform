-- Rollback of 000112: recreate the 9-column unique index exactly as defined by
-- 000109.
--
-- Only safe together with a code rollback to the pre-PR-C upsert (9-column
-- ON CONFLICT). PR-C code needs the 11-column arbiter, so rolling PR-C back
-- requires this down migration first, otherwise the old code runs without a
-- 9-column arbiter. The recommendations table is a regenerable cache, so rows
-- that differ only by platform or tenancy (allowed while the 9-column index is
-- absent) are collapsed to the most recently collected one before the index is
-- rebuilt; the next collection cycle repopulates the rest. Only duplicates
-- under the 9-column key are deleted.
DELETE FROM recommendations
WHERE id IN (
    SELECT id FROM (
        SELECT id, ROW_NUMBER() OVER (
            PARTITION BY account_key, provider, service, region, resource_type, engine, term, payment_option, availability_zone
            ORDER BY collected_at DESC, id DESC
        ) AS rn
        FROM recommendations
    ) ranked
    WHERE rn > 1
);

CREATE UNIQUE INDEX IF NOT EXISTS recommendations_natural_key_az_idx
    ON recommendations (account_key, provider, service, region, resource_type, engine, term, payment_option, availability_zone);
