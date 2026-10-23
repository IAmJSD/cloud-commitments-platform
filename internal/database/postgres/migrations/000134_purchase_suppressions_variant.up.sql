-- Migration 000134 (issue #816): widen the purchase suppression key with the
-- availability zone, platform and tenancy of the purchased recommendation.
--
-- The 6-tuple key (account, provider, service, region, resource type, engine)
-- made a purchase of one zonal EC2 variant subtract its count from every
-- sibling recommendation on that tuple (other AZs, the regional rec, other
-- platforms and tenancies). The recommendations natural key, rec ID and
-- identity key already carry these three fields (#792, #806).
--
-- Expand step, compatible with code that predates #816:
--   * The columns are nullable with no default, so adding them is a catalog
--     change and does not rewrite the table. Old code keeps inserting the
--     6-tuple only; its rows get NULL in all three columns.
--   * NULL in all three means "variant unknown": the reader treats the row
--     as a wildcard over its 6-tuple, which is the pre-#816 behavior. New code
--     always writes all three (''/'' is a known value, e.g. a regional or a
--     non-EC2 rec). The CHECK keeps the three columns all-NULL or all-set.
--   * The 6-tuple UNIQUE constraint is replaced by one that also covers the
--     three columns, so one execution can hold two rows that differ only by
--     AZ, platform or tenancy. Old code aggregates per 6-tuple before
--     inserting and never relies on the constraint (plain INSERT, no
--     ON CONFLICT), so dropping it is safe under old code. NULLS NOT DISTINCT
--     keeps legacy (all-NULL) rows unique per execution and 6-tuple, as before.
ALTER TABLE purchase_suppressions
    ADD COLUMN IF NOT EXISTS availability_zone TEXT,
    ADD COLUMN IF NOT EXISTS platform TEXT,
    ADD COLUMN IF NOT EXISTS tenancy TEXT;

ALTER TABLE purchase_suppressions
    ADD CONSTRAINT purchase_suppressions_variant_all_or_none
    CHECK ((availability_zone IS NULL) = (platform IS NULL)
       AND (platform IS NULL) = (tenancy IS NULL));

ALTER TABLE purchase_suppressions
    DROP CONSTRAINT IF EXISTS purchase_suppressions_execution_id_account_id_provider_serv_key;

ALTER TABLE purchase_suppressions
    ADD CONSTRAINT purchase_suppressions_execution_variant_key
    UNIQUE NULLS NOT DISTINCT (execution_id, account_id, provider, service, region,
                               resource_type, engine, availability_zone, platform, tenancy);
