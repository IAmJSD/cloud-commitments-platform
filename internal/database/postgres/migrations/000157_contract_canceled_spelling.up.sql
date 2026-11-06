-- Migration 000157: CONTRACT half of the 000089 'cancelled' -> 'canceled'
-- rename (issue #47), part 1 of 2.
--
-- 000089 (EXPAND) widened both status CHECKs to accept both spellings, added
-- canceled_by next to the legacy cancelled_by, and left normalization to this
-- step. Every build since #1277 writes only 'canceled', so the legacy set of
-- status rows is final and can be normalized and forbidden here.
--
-- This release still reads both spellings of the status and both actor
-- columns (COALESCE(canceled_by, cancelled_by)). It stops writing the legacy
-- actor column, but older builds that still write it keep working, so this
-- migration neither drains cancelled_by nor mirrors it. The drain, the strict
-- reads and the cancelled_by drop belong to a follow-up, shipped once this
-- migration has run everywhere and no build older than this release can be
-- rolled back to.
--
-- Concurrency: both tables are locked up front, so no writer can commit a row
-- between the normalize UPDATEs and the narrowed CHECKs (which would fail the
-- migration). golang-migrate runs this file in one transaction.
--
-- updated_at is the plan-health and retention clock. The row triggers stamp it
-- on every UPDATE, so they are disabled around the normalization: a pure
-- spelling change must not make old rows look fresh. DISABLE TRIGGER is
-- transactional; if any step fails, the whole file rolls back with the
-- triggers still enabled.

LOCK TABLE purchase_executions IN ACCESS EXCLUSIVE MODE;
LOCK TABLE ri_exchange_history IN ACCESS EXCLUSIVE MODE;

ALTER TABLE purchase_executions DISABLE TRIGGER update_purchase_executions_updated_at;
ALTER TABLE ri_exchange_history DISABLE TRIGGER ri_exchange_updated_at;

-- Normalize the data BEFORE narrowing, or the narrowed CHECKs reject the
-- surviving legacy rows.
UPDATE purchase_executions SET status = 'canceled' WHERE status = 'cancelled';
UPDATE ri_exchange_history SET status = 'canceled' WHERE status = 'cancelled';

ALTER TABLE purchase_executions ENABLE TRIGGER update_purchase_executions_updated_at;
ALTER TABLE ri_exchange_history ENABLE TRIGGER ri_exchange_updated_at;

-- Narrow purchase_executions: the 000089 list without 'cancelled'.
ALTER TABLE purchase_executions DROP CONSTRAINT IF EXISTS valid_status;
ALTER TABLE purchase_executions DROP CONSTRAINT IF EXISTS purchase_executions_status_check;
ALTER TABLE purchase_executions ADD CONSTRAINT purchase_executions_status_check
    CHECK (status IN (
        'pending','notified','approved','running','completed',
        'partially_completed','failed',
        'canceled',
        'expired','paused','revocation_requested','scheduled'
    ));

-- Narrow ri_exchange_history: the 000089 list without 'cancelled'.
ALTER TABLE ri_exchange_history DROP CONSTRAINT IF EXISTS ri_exchange_history_status_check;
ALTER TABLE ri_exchange_history ADD CONSTRAINT ri_exchange_history_status_check
    CHECK (status IN ('pending', 'processing', 'completed', 'failed', 'canceled'));
