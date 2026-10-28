-- Rollback of 000155: recreate the 11-column unique index exactly as defined
-- by 000111 and the source-account index exactly as defined by 000120.
--
-- Only safe together with a code rollback to the 11-column upsert. The newer
-- code needs the 12-column arbiter from 000154, which this rollback keeps. The
-- recommendations table is a regenerable cache, so rows that differ only by
-- az_config (allowed while the 11-column index is absent) are collapsed to the
-- most recently collected one before the indexes are rebuilt; the next
-- collection cycle repopulates the rest. Only duplicates under the 11-column
-- key are deleted, which also clears every duplicate under the finer 000120
-- source key.
DELETE FROM recommendations
WHERE id IN (
    SELECT id FROM (
        SELECT id, ROW_NUMBER() OVER (
            PARTITION BY account_key, provider, service, region, resource_type, engine, term, payment_option, availability_zone, platform, tenancy
            ORDER BY collected_at DESC, id DESC
        ) AS rn
        FROM recommendations
    ) ranked
    WHERE rn > 1
);

CREATE UNIQUE INDEX IF NOT EXISTS recommendations_natural_key_az_tp_idx
    ON recommendations (account_key, provider, service, region, resource_type, engine, term, payment_option, availability_zone, platform, tenancy);

CREATE UNIQUE INDEX IF NOT EXISTS recommendations_natural_key_source_idx
    ON recommendations (account_key, provider, service, region, resource_type, engine, term, payment_option, availability_zone, platform, tenancy, source_account_key);
