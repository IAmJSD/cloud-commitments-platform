-- Revert migration 000149: drop the RI-exchange range CHECKs. No data changes.
ALTER TABLE global_config
    DROP CONSTRAINT IF EXISTS chk_global_config_ri_exchange_max_per_exchange_usd_range,
    DROP CONSTRAINT IF EXISTS chk_global_config_ri_exchange_max_daily_usd_range,
    DROP CONSTRAINT IF EXISTS chk_global_config_ri_exchange_utilization_threshold_range;
