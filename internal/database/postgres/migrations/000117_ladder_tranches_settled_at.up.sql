-- Migration 000117 (platform#842/#843): when a ladder tranche became completed.
-- Netting and the amend ceiling need the completions that landed after a given
-- instant, because a completed tranche leaves the in-flight sum before it shows
-- in the existing commitment the run read. Nullable and additive.
-- TransitionLadderTrancheTx stamps it in the same UPDATE that sets completed.
--
-- Rows completed before this migration are backfilled with now(). That is never
-- earlier than the real settle time, so any "settled after anchor" sum counts
-- them for every anchor older than this migration: it errs toward counting
-- more commitment, never less. The backfill must come before the CHECK is
-- enforced: a CHECK, even NOT VALID, is re-evaluated on every UPDATE of a row,
-- including the ON DELETE SET NULL updates of config_id/run_id/execution_id,
-- so an unstamped completed row would make deleting its account, config or
-- execution fail. Idempotent.
ALTER TABLE ladder_tranches
    ADD COLUMN IF NOT EXISTS settled_at TIMESTAMPTZ;

UPDATE ladder_tranches
SET settled_at = now()
WHERE status = 'completed' AND settled_at IS NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'ladder_tranches_completed_has_settled_at'
          AND conrelid = 'ladder_tranches'::regclass
    ) THEN
        ALTER TABLE ladder_tranches
            ADD CONSTRAINT ladder_tranches_completed_has_settled_at
            CHECK (status <> 'completed' OR settled_at IS NOT NULL);
    END IF;
END $$;
