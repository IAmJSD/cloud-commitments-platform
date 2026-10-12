-- Rollback of 000110: recreate the 8-column unique index exactly as defined by
-- 000043.
--
-- Only safe together with a code rollback to the pre-PR2c upsert (8-column
-- ON CONFLICT). The recommendations table is a regenerable cache, so rows that
-- differ only by availability zone (allowed while the 8-column index is absent)
-- are collapsed to the most recently collected one before the index is rebuilt;
-- the next collection cycle repopulates the rest.
DELETE FROM recommendations
WHERE id IN (
    SELECT id FROM (
        SELECT id, ROW_NUMBER() OVER (
            PARTITION BY account_key, provider, service, region, resource_type, engine, term, payment_option
            ORDER BY collected_at DESC, id DESC
        ) AS rn
        FROM recommendations
    ) ranked
    WHERE rn > 1
);

CREATE UNIQUE INDEX IF NOT EXISTS recommendations_natural_key_idx
    ON recommendations (account_key, provider, service, region, resource_type, engine, term, payment_option);
