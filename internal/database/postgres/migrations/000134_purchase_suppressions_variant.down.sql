-- Rollback of 000134: restore the 6-tuple suppression key.
--
-- Rows of one execution that differ only by availability zone, platform or
-- tenancy (allowed while the widened key exists) are merged into one row per
-- execution and 6-tuple before the old UNIQUE constraint is recreated. The
-- merged row keeps the summed count and the latest expiry (rows of one
-- execution and provider share one expiry in practice), so the rollback never
-- suppresses less than before; it over-suppresses siblings, which is the
-- pre-#816 behavior that the old code expects.
WITH merged AS (
    SELECT execution_id, account_id, provider, service, region, resource_type, engine,
           SUM(suppressed_count)::INT AS total,
           MAX(expires_at) AS expires,
           MIN(created_at) AS created,
           (ARRAY_AGG(id ORDER BY created_at, id))[1] AS keep_id
    FROM purchase_suppressions
    GROUP BY execution_id, account_id, provider, service, region, resource_type, engine
    HAVING COUNT(*) > 1
),
dropped AS (
    DELETE FROM purchase_suppressions s
    USING merged m
    WHERE s.execution_id = m.execution_id
      AND s.account_id = m.account_id
      AND s.provider = m.provider
      AND s.service = m.service
      AND s.region = m.region
      AND s.resource_type = m.resource_type
      AND s.engine = m.engine
      AND s.id <> m.keep_id
)
UPDATE purchase_suppressions s
SET suppressed_count = m.total, expires_at = m.expires, created_at = m.created
FROM merged m
WHERE s.id = m.keep_id;

ALTER TABLE purchase_suppressions
    DROP CONSTRAINT IF EXISTS purchase_suppressions_execution_variant_key;

ALTER TABLE purchase_suppressions
    DROP CONSTRAINT IF EXISTS purchase_suppressions_variant_all_or_none;

ALTER TABLE purchase_suppressions
    DROP COLUMN IF EXISTS availability_zone,
    DROP COLUMN IF EXISTS platform,
    DROP COLUMN IF EXISTS tenancy;

ALTER TABLE purchase_suppressions
    ADD CONSTRAINT purchase_suppressions_execution_id_account_id_provider_serv_key
    UNIQUE (execution_id, account_id, provider, service, region, resource_type, engine);
