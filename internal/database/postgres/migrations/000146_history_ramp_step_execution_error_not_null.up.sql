-- Migration 000146: purchase_history.ramp_step and purchase_executions.error
-- become NOT NULL with a default (issue #1484).
--
-- Both columns have been nullable since 000001, but the Go readers scan them
-- into plain values: ramp_step into an int, error into a string (the error
-- scanner has tolerated NULL since #1473; the purchase_history readers do
-- not). The Go writers never store NULL, so a NULL can only come from a legacy
-- or hand-written row, and one such purchase_history row fails the scan of the
-- whole listing (GetAllPurchaseHistory and the other history readers).
--
-- The fix is at the schema: backfill NULL to the value the Go zero value
-- already means (ramp_step 0 = not part of a ramp step, error '' = no error),
-- then set NOT NULL with that value as the default.
--
-- ------------------------------------------------------------------
-- Concurrency
-- ------------------------------------------------------------------
-- The migration runs as one implicit transaction (see
-- migration_transactional_test.go). Both tables are locked ACCESS EXCLUSIVE
-- FIRST, in one statement, before the backfill. Without the lock a writer
-- still running an older build, or a hand-written INSERT, could commit a NULL
-- between the UPDATE and SET NOT NULL, and SET NOT NULL would then fail the
-- whole migration. Taking ACCESS EXCLUSIVE up front (rather than letting the
-- UPDATE take ROW EXCLUSIVE and the ALTER upgrade it later) also avoids a
-- lock upgrade that could deadlock with a reader that goes on to write.
-- Writers wait for the commit; the backfill only touches NULL rows and SET
-- NOT NULL is one scan, so the lock is short.
--
-- ------------------------------------------------------------------
-- updated_at
-- ------------------------------------------------------------------
-- update_purchase_executions_updated_at would stamp NOW() on every backfilled
-- execution row. updated_at is a lifecycle clock (plan health windows its
-- failed/canceled counts on it, CleanupOldExecutions ages canceled rows by
-- it; see 000128), and turning a NULL error into '' is not a lifecycle event.
-- So the trigger is disabled for the backfill statement only. DISABLE TRIGGER
-- names only that trigger and needs no superuser (the same pattern as 000127
-- and 000145); the file is one transaction, so it is re-enabled before commit
-- and a failure rolls both back. purchase_history has no UPDATE trigger.
--
-- Idempotent: re-running finds no NULL rows, and SET DEFAULT / SET NOT NULL
-- on a column that already has them are no-ops.

LOCK TABLE purchase_executions, purchase_history IN ACCESS EXCLUSIVE MODE;

UPDATE purchase_history SET ramp_step = 0 WHERE ramp_step IS NULL;

ALTER TABLE purchase_history
    ALTER COLUMN ramp_step SET DEFAULT 0,
    ALTER COLUMN ramp_step SET NOT NULL;

ALTER TABLE purchase_executions DISABLE TRIGGER update_purchase_executions_updated_at;

UPDATE purchase_executions SET error = '' WHERE error IS NULL;

ALTER TABLE purchase_executions ENABLE TRIGGER update_purchase_executions_updated_at;

ALTER TABLE purchase_executions
    ALTER COLUMN error SET DEFAULT '',
    ALTER COLUMN error SET NOT NULL;
