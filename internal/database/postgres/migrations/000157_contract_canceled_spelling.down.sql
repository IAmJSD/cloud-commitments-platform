-- Rollback 000157: re-widen both status CHECKs to the 000089 EXPAND state.
--
-- Rows stay 'canceled': the widened CHECKs accept both spellings and every
-- build since #1277 reads both, so converting them back is unnecessary.

LOCK TABLE purchase_executions IN ACCESS EXCLUSIVE MODE;
LOCK TABLE ri_exchange_history IN ACCESS EXCLUSIVE MODE;

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
