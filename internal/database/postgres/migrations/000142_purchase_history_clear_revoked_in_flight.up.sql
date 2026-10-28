-- Clear revocation_in_flight on purchase_history rows that are already
-- revoked (issue #316, A04-013).
--
-- Migration 000072 documents that a successful MarkPurchaseRevoked resets the
-- flag, but until #316 its UPDATE never did, so every Azure revocation that
-- completed left revocation_in_flight = true behind. Those rows read back as
-- in flight and never leave idx_purchase_history_revocation_in_flight.
-- MarkPurchaseRevoked now clears the flag; this brings existing rows in line.
--
-- Rows with revoked_at IS NULL are not touched: a flag on an unrevoked row is
-- the finalize_revocations sweep's signal that Azure refunded but the DB write
-- failed, and clearing it would lose that reconciliation.
--
-- Concurrency: nothing sets revoked_at back to NULL, so a row this statement
-- matches is finished and has nothing left in flight. A revoke request racing
-- the migration can at worst re-flip a revoked row; the handler reads
-- revoked_at first and that request's own failed Return clears it again.

UPDATE purchase_history
   SET revocation_in_flight = false
 WHERE revocation_in_flight
   AND revoked_at IS NOT NULL;
