-- Migration 000112: safety layer for plan-step auto-purchase (platform#631 PR2c).
--
-- purchase_plans.max_step_commitment: per-plan ceiling on the TOTAL commitment
-- (upfront + monthly * term * 12) one ramp step may buy. Nullable and additive:
-- NULL means "refuse auto-purchase for recommendation-bearing steps" (fail
-- closed), so every existing plan stays unable to buy.
--
-- global_config.auto_purchase_enabled: the global kill switch behind the
-- previously unread scheduling.auto_purchase default. Default FALSE (fail
-- closed): AutoPurchase plans never execute until an operator opts in.
--
-- Both statements are idempotent (ADD COLUMN IF NOT EXISTS) and metadata-only.
ALTER TABLE purchase_plans
    ADD COLUMN IF NOT EXISTS max_step_commitment NUMERIC(18,2)
    CHECK (max_step_commitment IS NULL OR max_step_commitment > 0);

ALTER TABLE global_config
    ADD COLUMN IF NOT EXISTS auto_purchase_enabled BOOLEAN NOT NULL DEFAULT FALSE;
