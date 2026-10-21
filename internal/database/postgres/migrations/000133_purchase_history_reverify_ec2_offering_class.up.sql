-- Clear stale EC2 RI offering_class stamps so they are re-read from AWS
-- (platform#524).
--
-- Before platform#509, savePurchaseHistory stamped every AWS 'ec2' row
-- 'convertible' whatever global_config.offering_class said. Rows bought while
-- that setting was 'standard' are Standard RIs recorded as convertible, so the
-- marketplace-sell guard refuses them and the History page hides Sell.
-- global_config keeps no history, so the database alone cannot tell those rows
-- from genuinely Convertible ones; only AWS DescribeReservedInstances can.
--
-- Owner option (a) on the issue: drop the convertible stamp on every AWS EC2
-- RI row (both the legacy 'ec2' and the canonical 'compute' slug, which also
-- clears any post-#509 re-drive stamp that recorded the class configured at
-- retry time). NULL is the column's "unknown" state: the marketplace-list
-- handler then calls populateOfferingClass, which fetches the class from AWS
-- and re-stamps the row, so a Standard RI becomes sellable and a Convertible
-- one is stamped convertible again after a single refused (400) Sell attempt.
--
-- Only 'convertible' is cleared. A 'standard' stamp is left alone (this issue
-- is about Standard RIs being blocked, not the reverse), and a concurrent
-- StampOfferingClass that commits 'standard' first is not overwritten:
-- under READ COMMITTED the UPDATE re-checks the WHERE clause on the
-- committed row version and skips it.
UPDATE purchase_history
SET offering_class = NULL
WHERE provider = 'aws'
  AND service IN ('ec2', 'compute')
  AND LOWER(offering_class) = 'convertible';
