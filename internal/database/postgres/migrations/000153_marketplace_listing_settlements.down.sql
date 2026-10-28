-- Revert migration 000153 (issue #1494): drop the per-listing settlement table
-- and the trigger that fills it. This restores the 000152 schema.
-- purchase_history's settlement columns were not changed by 000153 and keep
-- each RI's latest listing settlement. Lossy by design: settlements of
-- superseded listings exist only in this table and are dropped with it.
DROP TRIGGER IF EXISTS purchase_history_keep_superseded_listing ON purchase_history;
DROP FUNCTION IF EXISTS keep_superseded_marketplace_listing();
DROP TABLE IF EXISTS marketplace_listing_settlements;
