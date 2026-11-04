-- Reverse 000150 (issue #120). Rows from different accounts can share
-- (region, lookback_days), which the restored key cannot hold, so the cache is
-- emptied first; the next read refetches from Cost Explorer.
LOCK TABLE ri_utilization_cache IN ACCESS EXCLUSIVE MODE;

DELETE FROM ri_utilization_cache;

ALTER TABLE ri_utilization_cache DROP CONSTRAINT IF EXISTS ri_utilization_cache_pkey;
ALTER TABLE ri_utilization_cache DROP COLUMN IF EXISTS cloud_account_id;
ALTER TABLE ri_utilization_cache
    ADD CONSTRAINT ri_utilization_cache_pkey PRIMARY KEY (region, lookback_days);
