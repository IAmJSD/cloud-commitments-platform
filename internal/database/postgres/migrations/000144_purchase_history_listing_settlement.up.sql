-- Migration 000144: RI Marketplace listing settlement columns (issue #27).
--
-- Migration 000087 left these out because nothing populated them yet. The
-- marketplace status poller (finalize_revocations sweep) now writes them:
--
-- listed_at: when AWS created the current listing (the listing's CreateDate
-- from DescribeReservedInstancesListings). NULL until the poller has seen the
-- listing, and on rows that were never listed.
-- listing_proceeds_received: USD the seller receives for the instances AWS
-- reports as sold when the listing closes (or is canceled after a partial
-- sale), net of the AWS fee. Recorded once per listing; NULL until settled or
-- when the sale price could not be determined from the listing.
-- listing_fee_paid: the AWS Reserved Instance Marketplace service fee on those
-- sales (12% of the sale price). Recorded with listing_proceeds_received.
--
-- listing_price_schedule, also named in issue #27, already exists: migration
-- 000105 added it for the in-flight listing attempt (issue #525), so it is not
-- added (or dropped on rollback) here.
--
-- All columns are nullable and added with IF NOT EXISTS so the migration is
-- idempotent on re-apply.

ALTER TABLE purchase_history
    ADD COLUMN IF NOT EXISTS listed_at                 TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS listing_proceeds_received NUMERIC(12,2),
    ADD COLUMN IF NOT EXISTS listing_fee_paid          NUMERIC(12,2);
