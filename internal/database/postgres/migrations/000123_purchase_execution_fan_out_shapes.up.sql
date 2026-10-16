-- Migration 000123 (issue #97): pin the fan-out shape of each purchase
-- execution lineage.
--
-- The per-rec provider idempotency token is derived from the lineage key on the
-- single-account path and from "<lineage key>:<account id>" on the per-account
-- fan-out path. The executor used to choose the path from the plan's CURRENT
-- plan_accounts, so a plan that gained its first account (or lost its last one)
-- between an attempt and its Retry derived a token the provider had never seen
-- and bought the commitment again.
--
-- One row per lineage key (purchase_executions.idempotency_key, or the
-- execution_id for rows that predate migration 000066), written once by the
-- first execution attempt and never updated: every later attempt of the same
-- lineage takes the recorded shape instead of the live plan configuration.
CREATE TABLE IF NOT EXISTS purchase_execution_fan_out_shapes (
    idempotency_key TEXT PRIMARY KEY,
    shape TEXT NOT NULL CHECK (shape IN ('single', 'multi')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Backfill only what the data proves. A per-account row's lineage key is
-- "<root key>:<account id>" and only the fan-out path writes one, so its root
-- lineage fanned out. A root lineage with no per-account row is left unpinned
-- (its next attempt pins the live shape, the pre-#97 behavior): a fan-out
-- reaped before any per-account row was saved leaves no such row, so inferring
-- "single" from their absence could pin the wrong shape.
INSERT INTO purchase_execution_fan_out_shapes (idempotency_key, shape)
SELECT DISTINCT split_part(idempotency_key, ':', 1), 'multi'
  FROM purchase_executions
 WHERE idempotency_key LIKE '%:%'
   AND split_part(idempotency_key, ':', 1) <> ''
ON CONFLICT (idempotency_key) DO NOTHING;
