ALTER TABLE ladder_tranches
    DROP CONSTRAINT IF EXISTS ladder_tranches_completed_has_settled_at;
ALTER TABLE ladder_tranches
    DROP COLUMN IF EXISTS settled_at;
