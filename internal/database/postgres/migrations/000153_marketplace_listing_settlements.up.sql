-- Migration 000153: one settlement row per RI Marketplace listing (issue #1494).
--
-- purchase_history holds one listing per RI: listing_id, listing_state,
-- listed_at, listing_proceeds_received and listing_fee_paid describe the RI's
-- latest listing, and a relist (or the claim that starts one) moves the row on
-- to the new listing on purpose (issue #27). A listing that sold partially and
-- was canceled before a poll saw the sale therefore lost its proceeds when the
-- RI was relisted: the row no longer named it, so nothing polled it again.
--
-- marketplace_listing_settlements keeps every listing's settlement in its own
-- row, keyed by the AWS listing id (AWS listing ids are unique, and each one
-- belongs to exactly one RI):
--
--   purchase_history_id  the purchase_history row the listing was recorded on
--                        (for legacy duplicate rows of one RI, the earliest
--                        when the poller or the backfill creates the row, the
--                        first row updated when the trigger does)
--   purchase_id          the RI (reservation) id, for per-RI totals
--   status               the listing's state as last recorded (lowercase AWS
--                        ListingStatus)
--   listed_at            the listing's AWS CreateDate, once the poller saw it
--   proceeds_received    seller's net USD for the sold instances, NULL until
--   fee_paid             settled; the AWS fee on that sale. Set together.
--   currency             ISO currency of the two amounts (the poller only
--                        prices USD tiers)
--   settled_at           when the amounts were recorded
--
-- The reference cascades: a purchase_history row that is deleted takes its
-- listings' rows with it, as it takes its own settlement columns. No code path
-- deletes purchase_history rows today.
--
-- Writers:
--   * The marketplace poller (SettleMarketplaceListing) upserts the row of a
--     listing it read as closed or canceled. The upsert only fills a row whose
--     amounts are still NULL, so concurrent or repeated polls record a
--     listing's proceeds once.
--   * Trigger purchase_history_keep_superseded_listing copies a listing into
--     this table, as purchase_history last recorded it, at the moment the row
--     stops describing it: when listing_id changes, or when a list attempt's
--     claim sets listing_client_token (the claimed row keeps the previous
--     listing id but its state becomes the attempt's). ON CONFLICT DO NOTHING
--     keeps a row the poller already wrote. The poller keeps polling such a
--     listing from here until it is settled, with the same rules it uses for
--     purchase_history rows.
--
-- purchase_history's settlement columns keep their meaning: the latest
-- listing's settlement, which the poller still writes in the same transaction.
-- Per-RI totals across listings are read from this table only, never added to
-- purchase_history's columns, so no listing is counted twice.
--
-- Backfill: one row per listing that purchase_history currently records a
-- settlement for (listing_proceeds_received IS NOT NULL). Legacy duplicate
-- rows of one RI hold the same listing, so the earliest row is used.
--
-- Read-only detection query: listings purchase_history records a settlement
-- for that have no row here (expected to return nothing after this migration):
--
--   SELECT p.purchase_id, p.listing_id, p.listing_proceeds_received
--   FROM purchase_history p
--   WHERE p.listing_proceeds_received IS NOT NULL
--     AND COALESCE(p.listing_id, '') <> ''
--     AND NOT EXISTS (SELECT 1 FROM marketplace_listing_settlements s
--                     WHERE s.listing_id = p.listing_id);
--
-- Down migration: drops the trigger, its function and this table, which
-- restores the 000152 schema. purchase_history's columns were never changed,
-- so they still hold each RI's latest listing settlement; the settlements of
-- superseded listings exist only here and are lost on rollback.

CREATE TABLE IF NOT EXISTS marketplace_listing_settlements (
    listing_id          TEXT PRIMARY KEY CHECK (listing_id <> ''),
    purchase_history_id UUID NOT NULL REFERENCES purchase_history (id) ON DELETE CASCADE,
    purchase_id         VARCHAR(255) NOT NULL,
    status              TEXT NOT NULL,
    listed_at           TIMESTAMPTZ,
    proceeds_received   NUMERIC(12,2),
    fee_paid            NUMERIC(12,2),
    currency            CHAR(3) NOT NULL DEFAULT 'USD' CHECK (currency ~ '^[A-Z]{3}$'),
    settled_at          TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT marketplace_listing_settlements_amounts_check
        CHECK ((proceeds_received IS NULL) = (fee_paid IS NULL)
               AND (proceeds_received IS NULL) = (settled_at IS NULL))
);

CREATE INDEX IF NOT EXISTS idx_marketplace_listing_settlements_purchase
    ON marketplace_listing_settlements (purchase_id);

CREATE INDEX IF NOT EXISTS idx_marketplace_listing_settlements_history_row
    ON marketplace_listing_settlements (purchase_history_id);

INSERT INTO marketplace_listing_settlements (
    listing_id, purchase_history_id, purchase_id, status, listed_at,
    proceeds_received, fee_paid, settled_at
)
SELECT DISTINCT ON (listing_id)
       listing_id, id, purchase_id, lower(COALESCE(listing_state, '')), listed_at,
       listing_proceeds_received, COALESCE(listing_fee_paid, 0), now()
FROM purchase_history
WHERE listing_proceeds_received IS NOT NULL
  AND COALESCE(listing_id, '') <> ''
ORDER BY listing_id, created_at, id
ON CONFLICT (listing_id) DO NOTHING;

CREATE OR REPLACE FUNCTION keep_superseded_marketplace_listing()
RETURNS TRIGGER AS $$
BEGIN
    INSERT INTO marketplace_listing_settlements (
        listing_id, purchase_history_id, purchase_id, status, listed_at,
        proceeds_received, fee_paid, settled_at
    ) VALUES (
        OLD.listing_id, OLD.id, OLD.purchase_id, lower(OLD.listing_state), OLD.listed_at,
        OLD.listing_proceeds_received,
        CASE WHEN OLD.listing_proceeds_received IS NOT NULL THEN COALESCE(OLD.listing_fee_paid, 0) END,
        CASE WHEN OLD.listing_proceeds_received IS NOT NULL THEN now() END
    )
    ON CONFLICT (listing_id) DO NOTHING;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS purchase_history_keep_superseded_listing ON purchase_history;
CREATE TRIGGER purchase_history_keep_superseded_listing
    AFTER UPDATE OF listing_id, listing_client_token ON purchase_history
    FOR EACH ROW
    WHEN (COALESCE(OLD.listing_id, '') <> ''
          AND COALESCE(OLD.listing_state, '') <> ''
          AND OLD.listing_client_token IS NULL
          AND (NEW.listing_id IS DISTINCT FROM OLD.listing_id
               OR NEW.listing_client_token IS NOT NULL))
    EXECUTE FUNCTION keep_superseded_marketplace_listing();
