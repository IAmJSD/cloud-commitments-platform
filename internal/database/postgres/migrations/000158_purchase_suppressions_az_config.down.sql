-- Rollback of 000158: restore the 000134 suppression key without az_config.
--
-- Rows of one execution that differ only by az_config are merged into one row
-- per execution, 6-tuple and variant before the narrower UNIQUE constraint is
-- recreated. The merged row keeps the summed count and the latest expiry, so
-- the rollback never suppresses less than before; it over-suppresses the
-- sibling offering, which is the pre-#408 behavior old code expects.
WITH merged AS (
    SELECT execution_id, account_id, provider, service, region, resource_type, engine,
           availability_zone, platform, tenancy,
           SUM(suppressed_count)::INT AS total,
           MAX(expires_at) AS expires,
           MIN(created_at) AS created,
           (ARRAY_AGG(id ORDER BY created_at, id))[1] AS keep_id
    FROM purchase_suppressions
    GROUP BY execution_id, account_id, provider, service, region, resource_type, engine,
             availability_zone, platform, tenancy
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
      AND s.availability_zone IS NOT DISTINCT FROM m.availability_zone
      AND s.platform IS NOT DISTINCT FROM m.platform
      AND s.tenancy IS NOT DISTINCT FROM m.tenancy
      AND s.id <> m.keep_id
)
UPDATE purchase_suppressions s
SET suppressed_count = m.total, expires_at = m.expires, created_at = m.created
FROM merged m
WHERE s.id = m.keep_id;

ALTER TABLE purchase_suppressions
    DROP CONSTRAINT IF EXISTS purchase_suppressions_execution_variant_azc_key;

ALTER TABLE purchase_suppressions
    DROP CONSTRAINT IF EXISTS purchase_suppressions_az_config_needs_variant;

ALTER TABLE purchase_suppressions
    DROP COLUMN IF EXISTS az_config;

ALTER TABLE purchase_suppressions
    ADD CONSTRAINT purchase_suppressions_execution_variant_key
    UNIQUE NULLS NOT DISTINCT (execution_id, account_id, provider, service, region,
                               resource_type, engine, availability_zone, platform, tenancy);
