-- Migration 000153: CONTRACT half of the 000089 'cancelled' -> 'canceled'
-- rename (issue #47), part 1 of 2.
--
-- 000089 (EXPAND) widened both status CHECKs to accept both spellings, added
-- canceled_by next to the legacy cancelled_by, and left normalization to this
-- step. Every build since #1277 writes only 'canceled', so the legacy set of
-- status rows is final and can be normalized and forbidden here.
--
-- The legacy cancelled_by column is NOT dropped yet. The build that runs this
-- migration still has old instances serving during a rolling deploy, and a
-- code-only rollback (rollback.yml) can bring the previous build back with no
-- schema change. That previous build reads COALESCE(canceled_by, cancelled_by)
-- and writes cancelled_by, so dropping the column now would break every
-- execution read on those instances. The build that ships this migration no
-- longer reads or writes cancelled_by; the column drop is a later migration,
-- shipped once no older build can run.
--
-- Until then a trigger keeps canceled_by authoritative: whenever a write leaves
-- canceled_by NULL but sets cancelled_by (what an older build's
-- SavePurchaseExecution does), it copies cancelled_by into canceled_by. That
-- makes canceled_by equal to COALESCE(canceled_by, cancelled_by), the value
-- older builds read, so new code can read canceled_by alone.
--
-- Concurrency: both tables are locked up front, so no writer can commit a row
-- between the normalize/drain UPDATEs and the narrowed CHECKs (which would fail
-- the migration) or between the drain and the trigger (which would leave an
-- undrained actor). Writers wait for the migration and then see the trigger and
-- the narrowed CHECKs. golang-migrate runs this file in one transaction.

LOCK TABLE purchase_executions, ri_exchange_history IN ACCESS EXCLUSIVE MODE;

-- 1. Mirror legacy actor writes into canceled_by from now on.
CREATE OR REPLACE FUNCTION purchase_executions_mirror_legacy_cancel_actor()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    NEW.canceled_by := NEW.cancelled_by;
    RETURN NEW;
END $$;

DROP TRIGGER IF EXISTS purchase_executions_mirror_legacy_cancel_actor ON purchase_executions;
CREATE TRIGGER purchase_executions_mirror_legacy_cancel_actor
    BEFORE INSERT OR UPDATE ON purchase_executions
    FOR EACH ROW
    WHEN (NEW.canceled_by IS NULL AND NEW.cancelled_by IS NOT NULL)
    EXECUTE FUNCTION purchase_executions_mirror_legacy_cancel_actor();

-- 2. Normalize the data BEFORE narrowing, or the narrowed CHECKs reject the
--    surviving legacy rows.
UPDATE purchase_executions SET status = 'canceled' WHERE status = 'cancelled';
UPDATE ri_exchange_history SET status = 'canceled' WHERE status = 'cancelled';
UPDATE purchase_executions
SET canceled_by = cancelled_by
WHERE canceled_by IS NULL
  AND cancelled_by IS NOT NULL;

-- 3. Narrow purchase_executions: the 000089 list without 'cancelled'.
ALTER TABLE purchase_executions DROP CONSTRAINT IF EXISTS valid_status;
ALTER TABLE purchase_executions DROP CONSTRAINT IF EXISTS purchase_executions_status_check;
ALTER TABLE purchase_executions ADD CONSTRAINT purchase_executions_status_check
    CHECK (status IN (
        'pending','notified','approved','running','completed',
        'partially_completed','failed',
        'canceled',
        'expired','paused','revocation_requested','scheduled'
    ));

-- 4. Narrow ri_exchange_history: the 000089 list without 'cancelled'.
ALTER TABLE ri_exchange_history DROP CONSTRAINT IF EXISTS ri_exchange_history_status_check;
ALTER TABLE ri_exchange_history ADD CONSTRAINT ri_exchange_history_status_check
    CHECK (status IN ('pending', 'processing', 'completed', 'failed', 'canceled'));
