-- Revert migration 000144. listing_price_schedule belongs to 000105 and stays.
ALTER TABLE purchase_history
    DROP COLUMN IF EXISTS listed_at,
    DROP COLUMN IF EXISTS listing_proceeds_received,
    DROP COLUMN IF EXISTS listing_fee_paid;
