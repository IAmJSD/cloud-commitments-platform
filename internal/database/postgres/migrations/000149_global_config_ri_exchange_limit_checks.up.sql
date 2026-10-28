-- Migration 000149: range CHECKs on the RI-exchange limits in global_config
-- (issue #120, item 2b, database half).
--
-- 000010 added ri_exchange_max_per_exchange_usd, ri_exchange_max_daily_usd and
-- ri_exchange_utilization_threshold as DOUBLE PRECISION NOT NULL with no range
-- constraint. GlobalConfig.validateRIExchangeLimits (#1297) now rejects bad
-- values on PUT /api/config, but a manual SQL update, an older build still
-- running during a deploy, or a future direct-DB writer could still store a
-- negative money cap, which the exchange guard would then compare spend
-- against. These constraints hold the database to the same bounds:
--
--   * each USD cap is a finite amount >= 0. 0 stays valid: it is the "not
--     configured" state the exchange guard refuses to run under. The
--     `< 'Infinity'` half matters because a bare `>= 0` admits both
--     'Infinity' and 'NaN' (PostgreSQL sorts NaN above every other float), and
--     validateRIExchangeLimits rejects both;
--   * the utilization threshold is a percentage in [0, 100]. BETWEEN already
--     rejects NaN and both infinities.
--
-- ------------------------------------------------------------------
-- Existing rows that violate a bound
-- ------------------------------------------------------------------
-- This read-only query lists them (global_config holds one row, id = 1):
--
--   SELECT id, ri_exchange_max_per_exchange_usd, ri_exchange_max_daily_usd,
--          ri_exchange_utilization_threshold
--     FROM global_config
--    WHERE NOT (ri_exchange_max_per_exchange_usd >= 0
--               AND ri_exchange_max_per_exchange_usd < 'Infinity'::float8)
--       OR NOT (ri_exchange_max_daily_usd >= 0
--               AND ri_exchange_max_daily_usd < 'Infinity'::float8)
--       OR NOT (ri_exchange_utilization_threshold BETWEEN 0 AND 100);
--
-- The migration does NOT fail on such a row and does NOT rewrite it: choosing
-- a money cap is an operator decision, and failing here would leave the
-- database dirty and block every later migration over one config value. Each
-- constraint is added NOT VALID, which enforces it on every INSERT and UPDATE
-- from this point on, and is then VALIDATEd only if no existing row violates
-- it. A constraint with a violating row stays NOT VALID and the migration
-- raises a WARNING naming it. To finish, set the value through
-- PUT /api/config (or an UPDATE) to an amount in range, then run:
--
--   ALTER TABLE global_config VALIDATE CONSTRAINT <name>;
--
-- or re-run this file, which is idempotent. Constraints still unvalidated:
--
--   SELECT conname FROM pg_constraint
--    WHERE conrelid = 'global_config'::regclass AND NOT convalidated;
--
-- Until then the only effect of the bad row is that a write which keeps the
-- bad value is refused, and validateRIExchangeLimits already refuses those.
--
-- ------------------------------------------------------------------
-- Concurrency
-- ------------------------------------------------------------------
-- The file runs as one transaction (see migration_transactional_test.go). The
-- first ADD CONSTRAINT takes ACCESS EXCLUSIVE on global_config and holds it to
-- commit, so no write lands between adding a constraint, checking for
-- violating rows and validating it. A config write already in flight finishes
-- first and is then seen by the check; one that arrives later waits and is
-- checked against the new constraints. global_config is a single row, so the
-- lock is brief.
--
-- The pg_constraint guards follow 000048, so a re-run neither fails on an
-- existing constraint nor re-adds it.

DO $$
DECLARE
    c RECORD;
    violated BOOLEAN;
BEGIN
    FOR c IN
        SELECT * FROM (VALUES
            ('chk_global_config_ri_exchange_max_per_exchange_usd_range',
             'ri_exchange_max_per_exchange_usd >= 0 AND ri_exchange_max_per_exchange_usd < ''Infinity''::float8'),
            ('chk_global_config_ri_exchange_max_daily_usd_range',
             'ri_exchange_max_daily_usd >= 0 AND ri_exchange_max_daily_usd < ''Infinity''::float8'),
            ('chk_global_config_ri_exchange_utilization_threshold_range',
             'ri_exchange_utilization_threshold BETWEEN 0 AND 100')
        ) AS v(name, expr)
    LOOP
        IF NOT EXISTS (
            SELECT 1 FROM pg_constraint
             WHERE conrelid = 'global_config'::regclass AND conname = c.name
        ) THEN
            EXECUTE format('ALTER TABLE global_config ADD CONSTRAINT %I CHECK (%s) NOT VALID', c.name, c.expr);
        END IF;

        EXECUTE format('SELECT EXISTS (SELECT 1 FROM global_config WHERE NOT (%s))', c.expr) INTO violated;
        IF violated THEN
            RAISE WARNING 'global_config violates % (%); left NOT VALID, so new writes are checked but the existing row is kept', c.name, c.expr
                USING HINT = 'Set the value in range (PUT /api/config or UPDATE global_config), then run: ALTER TABLE global_config VALIDATE CONSTRAINT ' || c.name;
        ELSE
            EXECUTE format('ALTER TABLE global_config VALIDATE CONSTRAINT %I', c.name);
        END IF;
    END LOOP;
END $$;
