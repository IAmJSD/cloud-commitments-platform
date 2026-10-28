-- Rollback 000153: back to the 000089 EXPAND state.
--
-- Widen both status CHECKs again first, so nothing the data steps below touch
-- can violate them. Rows stay 'canceled': the widened CHECKs accept both
-- spellings and every build since #1277 reads both, so converting them back is
-- unnecessary.
--
-- Then drop the mirror trigger and copy canceled_by into the legacy
-- cancelled_by wherever it is missing, so the column holds what a build that
-- still dual-writes it would have left there.

LOCK TABLE purchase_executions, ri_exchange_history IN ACCESS EXCLUSIVE MODE;

ALTER TABLE purchase_executions DROP CONSTRAINT IF EXISTS purchase_executions_status_check;
ALTER TABLE purchase_executions ADD CONSTRAINT purchase_executions_status_check
    CHECK (status IN (
        'pending','notified','approved','running','completed',
        'partially_completed','failed',
        'cancelled','canceled',
        'expired','paused','revocation_requested','scheduled'
    ));

ALTER TABLE ri_exchange_history DROP CONSTRAINT IF EXISTS ri_exchange_history_status_check;
ALTER TABLE ri_exchange_history ADD CONSTRAINT ri_exchange_history_status_check
    CHECK (status IN ('pending', 'processing', 'completed', 'failed', 'cancelled', 'canceled'));

DROP TRIGGER IF EXISTS purchase_executions_mirror_legacy_cancel_actor ON purchase_executions;
DROP FUNCTION IF EXISTS purchase_executions_mirror_legacy_cancel_actor();

UPDATE purchase_executions
SET cancelled_by = canceled_by
WHERE cancelled_by IS NULL
  AND canceled_by IS NOT NULL;
