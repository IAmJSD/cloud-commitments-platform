-- 000147 down: restore the definitions that SUM/AVG over the nullable cost
-- columns and so skip unknown rows: monthly_savings_summary as 000078 left
-- it, provider_savings_summary as 000074 left it, with the plain-column
-- NULLS NOT DISTINCT unique indexes from 000076.
DROP MATERIALIZED VIEW IF EXISTS monthly_savings_summary CASCADE;

CREATE MATERIALIZED VIEW monthly_savings_summary AS
SELECT
    month,
    account_id,
    cloud_account_id,
    provider,
    service,
    AVG(ts_savings) as total_savings,
    AVG(ts_coverage) as avg_coverage,
    AVG(ts_commitment) as total_commitment,
    AVG(ts_usage) as total_usage,
    SUM(ts_row_count)::BIGINT as snapshot_count,
    MAX(timestamp) as last_updated
FROM (
    SELECT
        DATE_TRUNC('month', timestamp) as month,
        timestamp,
        account_id,
        cloud_account_id,
        provider,
        service,
        SUM(total_savings) as ts_savings,
        AVG(coverage_percentage) as ts_coverage,
        SUM(total_commitment) as ts_commitment,
        SUM(total_usage) as ts_usage,
        COUNT(*) as ts_row_count
    FROM savings_snapshots
    GROUP BY DATE_TRUNC('month', timestamp), timestamp,
             account_id, cloud_account_id, provider, service
) per_ts
GROUP BY month, account_id, cloud_account_id, provider, service;

CREATE UNIQUE INDEX idx_monthly_savings_summary_unique
    ON monthly_savings_summary (month, account_id, cloud_account_id, provider, service)
    NULLS NOT DISTINCT;

DROP MATERIALIZED VIEW IF EXISTS provider_savings_summary CASCADE;

CREATE MATERIALIZED VIEW provider_savings_summary AS
SELECT
    provider,
    account_id,
    cloud_account_id,
    MAX(ts_service_count) as service_count,
    AVG(ts_savings) as total_savings,
    AVG(ts_commitment) as total_commitment,
    AVG(ts_coverage) as avg_coverage,
    MAX(timestamp) as last_updated
FROM (
    SELECT
        provider,
        account_id,
        cloud_account_id,
        timestamp,
        SUM(total_savings) as ts_savings,
        SUM(total_commitment) as ts_commitment,
        AVG(coverage_percentage) as ts_coverage,
        COUNT(DISTINCT service) as ts_service_count
    FROM savings_snapshots
    WHERE timestamp > NOW() - INTERVAL '90 days'
    GROUP BY provider, account_id, cloud_account_id, timestamp
) per_ts
GROUP BY provider, account_id, cloud_account_id;

CREATE UNIQUE INDEX idx_provider_savings_summary_unique
    ON provider_savings_summary (provider, account_id, cloud_account_id)
    NULLS NOT DISTINCT;
