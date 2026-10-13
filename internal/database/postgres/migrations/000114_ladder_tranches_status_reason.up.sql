-- Migration 000114 (platform#118 PR-C2): why a ladder tranche reached a
-- terminal status ('expired', 'no_submission', 'approval_canceled', ...).
-- Nullable and additive; old code ignores it. Idempotent.
ALTER TABLE ladder_tranches
    ADD COLUMN IF NOT EXISTS status_reason TEXT;
