-- Re-key Azure/GCP service_configs and account_service_overrides rows still
-- stored under the Settings page's old service ids to the canonical slugs
-- (issue #613).
--
-- Recommendations carry the provider's common.ServiceType slug, and the
-- scheduler (config.ResolveAccountConfigsForRecs) and the purchase modal look
-- up both tables by that exact (provider, service) pair. Before #1354 the
-- Settings page saved Azure and GCP rows under per-product ids that no
-- recommendation carries, so those defaults and overrides never applied. #1354
-- changed what the page writes and re-keys old rows only in the browser; the
-- stored rows stayed under the old ids. This migration rewrites them.
--
-- The mapping is LEGACY_SETTINGS_SERVICES in
-- frontend/src/lib/settings-service-ids.ts:
--   azure/vm -> compute, azure/sql -> relational-db, azure/cosmosdb -> nosql,
--   azure/redis -> cache, gcp/sql -> relational-db, gcp/memorystore -> cache.
-- (Plans used gcp/cloudsql, not gcp/sql; 000136 rewrote those. Settings never
-- wrote cloudsql, so it is not mapped here.)
--
-- Precedence is the same as canonicalizeServiceConfigs in that file: when a
-- row already exists under the canonical id (the same provider and service,
-- and for overrides the same account), that row wins and the legacy row is
-- deleted, since it never applied. Otherwise the legacy row is re-keyed in
-- place: id, settings and created_at stay, and updated_at is bumped by the
-- tables' update triggers. The mapping sends no two legacy ids of a provider
-- to the same canonical id, so two re-keys cannot collide.
--
-- Audit trail: every override this migration deletes or re-keys gets
-- config_audit_events rows (000147, issue #1493), with actor
-- 'migration 000151' and details naming the migration and the legacy and
-- canonical ids:
--   * a deleted legacy twin: one account_service_override.delete event whose
--     before is the deleted row's settings, so the dropped values can be
--     recovered by hand;
--   * a re-key: a delete event under the legacy id and an upsert (create)
--     event under the canonical id, both carrying the same settings.
-- before/after use overrideSnapshotJSON's shape (internal/config). There is
-- no audit trail for service_configs, so those rewrites record nothing.
--
-- Concurrency: application instances serve requests while a new instance runs
-- this migration (RunMigrations runs on demand). The file locks both tables
-- IN EXCLUSIVE MODE first. That waits for any open transaction that has
-- written either table, so a legacy row it commits is rewritten too, and it
-- makes later writers wait, including their SELECT ... FOR UPDATE reads
-- (UpdateServiceConfigAtomic, SaveAccountServiceOverrideAuditedTx), so a
-- writer that queues behind the migration reads and merges onto the
-- rewritten row. Plain reads are not blocked. Both tables are small (one row
-- per provider/service, and per account for overrides).
--
-- Detection query for live databases (read only), listing every row this
-- migration rewrites and whether a canonical twin will win over it:
--
--   WITH alias(provider, legacy, canonical) AS (VALUES
--       ('azure', 'vm', 'compute'), ('azure', 'sql', 'relational-db'),
--       ('azure', 'cosmosdb', 'nosql'), ('azure', 'redis', 'cache'),
--       ('gcp', 'sql', 'relational-db'), ('gcp', 'memorystore', 'cache'))
--   SELECT 'service_configs' AS tbl, NULL::uuid AS account_id, s.provider,
--          s.service, a.canonical,
--          EXISTS (SELECT 1 FROM service_configs c
--                   WHERE c.provider = s.provider AND c.service = a.canonical) AS twin_wins
--     FROM service_configs s
--     JOIN alias a ON a.provider = s.provider AND a.legacy = s.service
--   UNION ALL
--   SELECT 'account_service_overrides', o.account_id, o.provider,
--          o.service, a.canonical,
--          EXISTS (SELECT 1 FROM account_service_overrides c
--                   WHERE c.account_id = o.account_id AND c.provider = o.provider
--                     AND c.service = a.canonical)
--     FROM account_service_overrides o
--     JOIN alias a ON a.provider = o.provider AND a.legacy = o.service
--    ORDER BY 1, 2, 3, 4;
--
-- Idempotent: a second run finds no legacy rows and writes nothing, not even
-- audit events. The helper functions are dropped before the file commits.
--
-- The down migration is a no-op; see its header for what it cannot restore.

LOCK TABLE service_configs, account_service_overrides IN EXCLUSIVE MODE;

CREATE FUNCTION legacy_settings_services_000151()
RETURNS TABLE (provider TEXT, legacy TEXT, canonical TEXT)
LANGUAGE sql IMMUTABLE AS $$
    VALUES ('azure', 'vm', 'compute'),
           ('azure', 'sql', 'relational-db'),
           ('azure', 'cosmosdb', 'nosql'),
           ('azure', 'redis', 'cache'),
           ('gcp', 'sql', 'relational-db'),
           ('gcp', 'memorystore', 'cache')
$$;

-- The override settings as overrideSnapshotJSON writes them. coverage goes
-- through float8 because the Go side scans it into a float64.
CREATE FUNCTION override_snapshot_000151(o account_service_overrides)
RETURNS JSONB LANGUAGE sql IMMUTABLE AS $$
    SELECT jsonb_build_object(
        'enabled', o.enabled,
        'term', o.term,
        'payment', o.payment,
        'coverage', o.coverage::float8,
        'ramp_schedule', o.ramp_schedule,
        'include_engines', o.include_engines,
        'exclude_engines', o.exclude_engines,
        'include_regions', o.include_regions,
        'exclude_regions', o.exclude_regions,
        'include_types', o.include_types,
        'exclude_types', o.exclude_types)
$$;

-- service_configs: the canonical twin wins, then the remaining legacy rows
-- are re-keyed.
DELETE FROM service_configs l
 USING legacy_settings_services_000151() a
 WHERE l.provider = a.provider AND l.service = a.legacy
   AND EXISTS (SELECT 1 FROM service_configs c
                WHERE c.provider = a.provider AND c.service = a.canonical);

UPDATE service_configs l
   SET service = a.canonical
  FROM legacy_settings_services_000151() a
 WHERE l.provider = a.provider AND l.service = a.legacy;

-- account_service_overrides: the same two steps, per account, each recording
-- its audit events.
WITH superseded AS (
    DELETE FROM account_service_overrides l
     USING legacy_settings_services_000151() a
     WHERE l.provider = a.provider AND l.service = a.legacy
       AND EXISTS (SELECT 1 FROM account_service_overrides c
                    WHERE c.account_id = l.account_id AND c.provider = a.provider
                      AND c.service = a.canonical)
    RETURNING l.account_id, l.provider, a.legacy, a.canonical, override_snapshot_000151(l) AS settings
)
INSERT INTO config_audit_events (id, event_type, actor, account_id, provider, service, before, after, details)
SELECT uuid_generate_v4(), 'account_service_override.delete', 'migration 000151',
       s.account_id, s.provider, s.legacy, s.settings, NULL,
       jsonb_build_object('migration', '000151',
                          'reason', 'legacy service id superseded by the canonical override',
                          'legacy_service', s.legacy, 'canonical_service', s.canonical)
  FROM superseded s
 ORDER BY s.account_id, s.provider, s.legacy;

WITH rekeyed AS (
    UPDATE account_service_overrides l
       SET service = a.canonical
      FROM legacy_settings_services_000151() a
     WHERE l.provider = a.provider AND l.service = a.legacy
    RETURNING l.account_id, l.provider, a.legacy, a.canonical, override_snapshot_000151(l) AS settings
)
INSERT INTO config_audit_events (id, event_type, actor, account_id, provider, service, before, after, details)
SELECT uuid_generate_v4(), e.event_type, 'migration 000151',
       r.account_id, r.provider, e.service, e.before, e.after,
       jsonb_build_object('migration', '000151',
                          'reason', 'legacy service id re-keyed to the canonical id',
                          'legacy_service', r.legacy, 'canonical_service', r.canonical)
  FROM rekeyed r
 CROSS JOIN LATERAL (VALUES
       (1, 'account_service_override.delete', r.legacy, r.settings, NULL::jsonb),
       (2, 'account_service_override.upsert', r.canonical, NULL::jsonb, r.settings)
 ) AS e(step, event_type, service, before, after)
 ORDER BY r.account_id, r.provider, r.legacy, e.step;

DROP FUNCTION override_snapshot_000151(account_service_overrides);
DROP FUNCTION legacy_settings_services_000151();
