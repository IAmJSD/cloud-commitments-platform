-- Record whether an Azure Return was actually accepted (issue #1523).
--
-- The finalize_revocations sweep used to mark every flagged, unrevoked
-- purchase_history row revoked. A row is also left flagged when a request
-- crashes after FlipPurchaseRevocationInFlight but before Azure accepts the
-- Return (or when the flip's commit result is lost), so the sweep could mark a
-- purchase revoked that was never refunded and is still billed.
--
--   return_posted_at         stamped by the revoke handler right after Azure
--                            accepts the Return, before MarkPurchaseRevoked.
--                            The sweep finalizes only rows that have it.
--   revocation_in_flight_at  stamped by FlipPurchaseRevocationInFlight. A row
--                            still flagged with no return_posted_at long after
--                            this has had its flag cleared by the sweep, so
--                            the purchase can be revoked again.
--
-- Rows already flagged when this lands are not backfilled: no Return was
-- recorded for them, so they are treated as "Return not recorded". The sweep
-- never finalizes them. It stamps revocation_in_flight_at the first time it
-- sees such a row and clears the flag once the timeout has passed from then.
-- Nothing here changes revocation_in_flight or revoked_at.

ALTER TABLE purchase_history
    ADD COLUMN IF NOT EXISTS return_posted_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS revocation_in_flight_at TIMESTAMPTZ;

-- The sweep's finalize queue: Return accepted, revocation not yet recorded.
CREATE INDEX IF NOT EXISTS idx_purchase_history_return_posted_unrevoked
    ON purchase_history (purchase_id)
    WHERE return_posted_at IS NOT NULL AND revoked_at IS NULL;
