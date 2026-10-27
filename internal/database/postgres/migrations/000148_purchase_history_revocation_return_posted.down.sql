-- Reverse 000148 (issue #1523). Lossy: which unrevoked rows had an accepted
-- Return recorded is dropped with the column. revocation_in_flight is left as
-- it is, so the pre-000148 sweep will mark every flagged, unrevoked row
-- revoked again.
DROP INDEX IF EXISTS idx_purchase_history_return_posted_unrevoked;
ALTER TABLE purchase_history
    DROP COLUMN IF EXISTS revocation_in_flight_at,
    DROP COLUMN IF EXISTS return_posted_at;
