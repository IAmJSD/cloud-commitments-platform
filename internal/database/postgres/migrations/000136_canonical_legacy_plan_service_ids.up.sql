-- Rewrite the service ids that plans saved before #608 still store to the
-- canonical slugs (issue #1124).
--
-- The pre-#608 plan modal offered per-provider option ids (Azure vm, sql,
-- redis and GCP cloudsql, memorystore) and the API accepted any non-empty
-- service, so such plans store e.g. {"provider":"azure","service":"vm"}.
-- mapServiceSlug knows only the canonical slugs and the legacy AWS ones, and
-- plan step resolution filters recommendations on the stored service, so a
-- stored "vm" matches no Azure recommendation (they carry "compute") and the
-- plan silently buys nothing. #1123 fixed only the edit path.
--
-- The mapping is the same table as legacyServiceAlias in
-- internal/api/plan_services_merge.go and LEGACY_PLAN_SERVICE_IDS in
-- frontend/src/plans.ts:
--   azure/vm -> compute, azure/sql -> relational-db, azure/redis -> cache,
--   gcp/cloudsql -> relational-db, gcp/memorystore -> cache.
-- Only the entry's "service" value changes. Keys stay as they are, because
-- #1123's singleton-alias edit path keeps a plan's original key.
--
-- An entry is left alone when another entry of the same plan already targets
-- the same provider and canonical service (a canonical sibling, or a second
-- copy of the same legacy id under another key). Rewriting it would make two
-- plan services match the same recommendations, which plan step resolution
-- rejects for the whole plan ("matches more than one plan service"), so such
-- plans keep today's behavior and need a manual decision.
--
-- The rewrite is one UPDATE per row with the same effect as editing the plan
-- through the API (PostgresStore.UpdatePurchasePlan):
--   * updated_at is bumped by update_purchase_plans_updated_at, so a plan
--     snapshot an application instance read before this migration fails its
--     optimistic-lock check (ErrPurchasePlanConflict) instead of writing the
--     legacy id back.
--   * When the enabled-service shape changes, ramp_baseline is cleared, as
--     UpdatePurchasePlan does via rampBaselineStale; ramp_ledger_shape is that
--     check's SQL mirror. guard_ramp_ledger_state stays enabled and applies
--     its usual rules: a never-started 'new' ramp stays 'new', and a plan that
--     had a baseline becomes 'legacy' (fail closed, as an API edit would).
--   * Rows are only touched when the rewrite changes them.
--
-- Detection query for live databases (read only), listing every legacy entry
-- this migration considers:
--
--   SELECT p.id, p.name, p.enabled, e.key,
--          e.value ->> 'provider' AS provider, e.value ->> 'service' AS service,
--          e.value ->> 'enabled' AS service_enabled
--     FROM purchase_plans p
--     CROSS JOIN LATERAL jsonb_each(CASE WHEN jsonb_typeof(p.services) = 'object'
--                                        THEN p.services ELSE '{}'::jsonb END) e
--    WHERE (e.value ->> 'provider', e.value ->> 'service') IN (
--          ('azure', 'vm'), ('azure', 'sql'), ('azure', 'redis'),
--          ('gcp', 'cloudsql'), ('gcp', 'memorystore'))
--    ORDER BY p.id, e.key;
--
-- The helper function is dropped again before the migration commits.

CREATE OR REPLACE FUNCTION canonical_plan_services_000136(services JSONB) RETURNS JSONB
LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN
    IF services IS NULL OR jsonb_typeof(services) <> 'object' THEN
        RETURN services;
    END IF;
    RETURN COALESCE((
        WITH alias(provider, legacy, canonical) AS (VALUES
            ('azure', 'vm', 'compute'),
            ('azure', 'sql', 'relational-db'),
            ('azure', 'redis', 'cache'),
            ('gcp', 'cloudsql', 'relational-db'),
            ('gcp', 'memorystore', 'cache')
        ), entry AS (
            SELECT e.key, e.value, e.value ->> 'provider' AS provider,
                   COALESCE(a.canonical, e.value ->> 'service') AS target,
                   a.canonical IS NOT NULL AS legacy
              FROM jsonb_each(services) e
              LEFT JOIN alias a
                ON a.provider = e.value ->> 'provider' AND a.legacy = e.value ->> 'service'
        )
        SELECT jsonb_object_agg(x.key, CASE
                   WHEN x.legacy AND NOT EXISTS (
                       SELECT 1 FROM entry y
                        WHERE y.key <> x.key AND y.provider = x.provider AND y.target = x.target)
                   THEN jsonb_set(x.value, '{service}', to_jsonb(x.target))
                   ELSE x.value
               END)
          FROM entry x
    ), services);
END
$$;

UPDATE purchase_plans
SET services = canonical_plan_services_000136(services),
    ramp_baseline = CASE
        WHEN ramp_ledger_shape(services, ramp_schedule)
             IS DISTINCT FROM ramp_ledger_shape(canonical_plan_services_000136(services), ramp_schedule)
        THEN NULL
        ELSE ramp_baseline
    END
WHERE services IS DISTINCT FROM canonical_plan_services_000136(services);

DROP FUNCTION canonical_plan_services_000136(JSONB);
