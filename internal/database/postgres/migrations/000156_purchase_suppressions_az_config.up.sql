-- Migration 000156 (issue #408): widen the purchase suppression variant with
-- the AWS RDS az_config ("single-az" / "multi-az").
--
-- Migration 000155 lets a Single-AZ and a Multi-AZ RDS rec of the same class,
-- engine, term and payment coexist, but the suppression key (000134) cannot
-- tell them apart: buying 2x Single-AZ would write a suppression that also
-- matched, and hid, the Multi-AZ rec. The variant now carries az_config for
-- AWS RDS recs only; every other provider/service writes NULL.
--
-- Backfill rule (none needed): az_config NULL means "unknown or not
-- applicable" and is a WILDCARD over the row's other variant columns, so every
-- row written before this migration keeps suppressing exactly what it
-- suppressed before (both the Single-AZ and the Multi-AZ rec). That fails safe
-- against a double purchase; the over-suppression ends when the row expires.
-- Rows are deliberately not rewritten: their original az_config is unknowable.
--
-- Expand step, compatible with code that predates this change: the column is
-- nullable with no default (catalog-only, no table rewrite) and old code's
-- inserts leave it NULL. The UNIQUE key gains the column (NULLS NOT DISTINCT,
-- so NULL rows stay unique per execution, 6-tuple and variant as before).
-- A suppression with an az_config must also carry the rest of the variant.
--
-- DEPLOY ORDER: apply this before deploying the code that writes az_config
-- (the insert names the column). Apply 000154, 000155 and this migration as
-- separate steps, each after the code it needs is live (see the 000155 header);
-- never dispatch database-migration.yml with the default of all pending steps.
ALTER TABLE purchase_suppressions
    ADD COLUMN IF NOT EXISTS az_config TEXT;

ALTER TABLE purchase_suppressions
    ADD CONSTRAINT purchase_suppressions_az_config_needs_variant
    CHECK (az_config IS NULL OR availability_zone IS NOT NULL);

ALTER TABLE purchase_suppressions
    DROP CONSTRAINT IF EXISTS purchase_suppressions_execution_variant_key;

ALTER TABLE purchase_suppressions
    ADD CONSTRAINT purchase_suppressions_execution_variant_azc_key
    UNIQUE NULLS NOT DISTINCT (execution_id, account_id, provider, service, region,
                               resource_type, engine, availability_zone, platform, tenancy,
                               az_config);
