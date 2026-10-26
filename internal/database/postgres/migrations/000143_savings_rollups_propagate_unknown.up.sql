-- 000143: keep an unknown cost unknown through the savings rollups (#151).
--
-- savings_snapshots.total_usage (000067, H2) and total_commitment (000101)
-- are nullable on purpose: the collector writes NULL when no provider figure
-- exists (for example an all-upfront purchase has no recurring cost to derive
-- usage from), because a placeholder 0 would read as a real $0. SUM and AVG
-- skip NULL, so the rollups below turned "us-east-1 $4000 + eu-west-1
-- unknown" into a definite $4000 and dropped the unknown region without any
-- signal.
--
-- Each rollup now yields NULL when any row it covers is NULL, in the same
-- COUNT(col) = COUNT(*) form the history totals use for upfront_cost
-- (internal/api/analytics_postgres.go). The inner query makes a timestamp's
-- total unknown if one of its rows is unknown; the outer query makes the
-- period's average unknown if one of its timestamps is, since averaging only
-- the known instants would understate a period in which a purchase with no
-- figure became active. A known 0 is still 0. total_savings is NOT NULL and
-- coverage_percentage is a percentage averaged over the rows that have one,
-- so both keep their current definitions.
--
-- daily_savings_trend aggregates no nullable cost column and is unchanged.
--
-- DROP + CREATE because a materialized view's query cannot be altered in
-- place. Both views are created populated, and their unique indexes are
-- recreated as 000076 left them, so refresh_savings_materialized_views() can
-- keep refreshing them CONCURRENTLY. The column names and types are
-- unchanged (the CASE has no ELSE, so it keeps the numeric type of SUM/AVG).
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
    CASE WHEN COUNT(ts_commitment) = COUNT(*) THEN AVG(ts_commitment) END as total_commitment,
    CASE WHEN COUNT(ts_usage) = COUNT(*) THEN AVG(ts_usage) END as total_usage,
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
        CASE WHEN COUNT(total_commitment) = COUNT(*) THEN SUM(total_commitment) END as ts_commitment,
        CASE WHEN COUNT(total_usage) = COUNT(*) THEN SUM(total_usage) END as ts_usage,
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
    CASE WHEN COUNT(ts_commitment) = COUNT(*) THEN AVG(ts_commitment) END as total_commitment,
    AVG(ts_coverage) as avg_coverage,
    MAX(timestamp) as last_updated
FROM (
    SELECT
        provider,
        account_id,
        cloud_account_id,
        timestamp,
        SUM(total_savings) as ts_savings,
        CASE WHEN COUNT(total_commitment) = COUNT(*) THEN SUM(total_commitment) END as ts_commitment,
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
