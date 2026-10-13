-- Migration 000118 (platform#631): the per-identity base a ramp plan sizes its
-- cumulative target against, frozen at step 1. A JSON object of identity key
-- to {count, hourly}. Nullable, no backfill: plans that never wrote one keep
-- sizing as before. Written only by SetPlanRampBaselineTx (insert-if-null) and
-- never by the full-row plan UPDATE, so a stale in-memory plan cannot clear it.
-- Idempotent.
ALTER TABLE purchase_plans
    ADD COLUMN IF NOT EXISTS ramp_baseline JSONB;
