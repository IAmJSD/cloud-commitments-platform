-- Revert 000132 (issue #912): restore the 000003 bodies of
-- create_savings_snapshot_partition and create_future_savings_partitions.
--
-- Partitions that the up migration rebuilt keep their UTC bounds. Those bounds
-- are what the collector assumes, and recreating the old session-zone bounds
-- would need the zone each partition was originally made in, which is not
-- recorded anywhere.

-- Function to create monthly partition for savings_snapshots
CREATE OR REPLACE FUNCTION create_savings_snapshot_partition(partition_date DATE)
RETURNS void AS $$
DECLARE
    partition_name TEXT;
    start_date DATE;
    end_date DATE;
BEGIN
    -- Calculate partition boundaries
    start_date := DATE_TRUNC('month', partition_date);
    end_date := start_date + INTERVAL '1 month';

    -- Generate partition table name (e.g., savings_snapshots_2024_01)
    partition_name := 'savings_snapshots_' || TO_CHAR(start_date, 'YYYY_MM');

    -- Check if partition already exists
    IF NOT EXISTS (
        SELECT 1 FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE c.relname = partition_name AND n.nspname = 'public'
    ) THEN
        -- Create partition
        EXECUTE format(
            'CREATE TABLE %I PARTITION OF savings_snapshots FOR VALUES FROM (%L) TO (%L)',
            partition_name,
            start_date,
            end_date
        );

        -- Create partition-specific index for faster queries
        EXECUTE format(
            'CREATE INDEX %I ON %I (timestamp DESC, account_id)',
            'idx_' || partition_name || '_timestamp',
            partition_name
        );

        RAISE NOTICE 'Created partition: %', partition_name;
    ELSE
        RAISE NOTICE 'Partition % already exists', partition_name;
    END IF;
END;
$$ LANGUAGE plpgsql;

-- Function to automatically create partitions for the next N months
CREATE OR REPLACE FUNCTION create_future_savings_partitions(months_ahead INTEGER DEFAULT 3)
RETURNS void AS $$
DECLARE
    i INTEGER;
    partition_date DATE;
BEGIN
    -- Create partitions for current month + N months ahead
    FOR i IN 0..months_ahead LOOP
        partition_date := DATE_TRUNC('month', CURRENT_DATE) + (i || ' months')::INTERVAL;
        PERFORM create_savings_snapshot_partition(partition_date);
    END LOOP;
END;
$$ LANGUAGE plpgsql;
