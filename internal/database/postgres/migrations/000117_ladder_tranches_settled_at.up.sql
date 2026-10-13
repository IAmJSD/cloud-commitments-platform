-- Migration 000117 (platform#842/#843): when a ladder tranche became completed.
-- Netting and the amend ceiling need the completions that landed after a given
-- instant, because a completed tranche leaves the in-flight sum before it shows
-- in the existing commitment the run read. Nullable and additive.
-- TransitionLadderTrancheTx stamps it in the same UPDATE that sets completed.
-- The CHECK is NOT VALID: rows completed before this migration keep a NULL
-- settled_at (consumers count them as settled-after-any-anchor, fail closed),
-- while every new or updated row must carry the stamp. Idempotent.
ALTER TABLE ladder_tranches
    ADD COLUMN IF NOT EXISTS settled_at TIMESTAMPTZ;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'ladder_tranches_completed_has_settled_at'
          AND conrelid = 'ladder_tranches'::regclass
    ) THEN
        ALTER TABLE ladder_tranches
            ADD CONSTRAINT ladder_tranches_completed_has_settled_at
            CHECK (status <> 'completed' OR settled_at IS NOT NULL) NOT VALID;
    END IF;
END $$;
