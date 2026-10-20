-- Migration 000132 (issue #912): cut savings_snapshots partitions at UTC month
-- starts whatever the session TimeZone is.
--
-- 000003 passed DATE bounds to FOR VALUES FROM (%L) TO (%L). The partition key
-- is TIMESTAMPTZ, so Postgres read those literals in the session's TimeZone:
-- a Europe/Berlin session cut December at 2026-11-30 23:00Z, an
-- America/Los_Angeles one at 2026-12-01 08:00Z (and 07:00Z for an April
-- boundary, DST). Neighbors made in different zones then leave gaps (rows fall
-- into savings_snapshots_default) or overlap (CREATE fails with "would
-- overlap"), and the collector stamps rows in UTC months.
--
-- The bounds are now written as explicit '+00' literals, which parse the same
-- under every TimeZone and DateStyle, and "today" is taken in UTC instead of
-- from CURRENT_DATE. The function also takes a transaction-scoped advisory lock
-- so two concurrent callers for the same month serialize: the second sees the
-- first's partition and returns, instead of failing with "relation already
-- exists" from the check-then-create race.

CREATE OR REPLACE FUNCTION create_savings_snapshot_partition(partition_date DATE)
RETURNS void AS $$
DECLARE
    partition_name TEXT;
    start_date DATE;
    end_date DATE;
BEGIN
    -- ::timestamp keeps DATE_TRUNC off the timestamptz overload, so no step
    -- here reads the session TimeZone.
    start_date := DATE_TRUNC('month', partition_date::timestamp)::date;
    end_date := (start_date + INTERVAL '1 month')::date;

    -- Generate partition table name (e.g., savings_snapshots_2024_01)
    partition_name := 'savings_snapshots_' || TO_CHAR(start_date, 'YYYY_MM');

    -- Serialize partition DDL across sessions; released at transaction end.
    PERFORM pg_advisory_xact_lock(hashtext('savings_snapshots_partition_ddl'));

    IF NOT EXISTS (
        SELECT 1 FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE c.relname = partition_name AND n.nspname = 'public'
    ) THEN
        EXECUTE format(
            'CREATE TABLE %I PARTITION OF savings_snapshots FOR VALUES FROM (%L) TO (%L)',
            partition_name,
            TO_CHAR(start_date, 'YYYY-MM-DD') || ' 00:00:00+00',
            TO_CHAR(end_date, 'YYYY-MM-DD') || ' 00:00:00+00'
        );

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

CREATE OR REPLACE FUNCTION create_future_savings_partitions(months_ahead INTEGER DEFAULT 3)
RETURNS void AS $$
DECLARE
    i INTEGER;
    partition_date DATE;
BEGIN
    -- Current UTC month + N months ahead; CURRENT_DATE is the session's date.
    FOR i IN 0..months_ahead LOOP
        partition_date := (DATE_TRUNC('month', now() AT TIME ZONE 'UTC')
                           + make_interval(months => i))::date;
        PERFORM create_savings_snapshot_partition(partition_date);
    END LOOP;
END;
$$ LANGUAGE plpgsql;

-- Rebuild any existing monthly partition whose bounds are not the UTC month
-- its name says. Its rows, and any default-partition rows that belong to the
-- affected UTC months, are copied aside, the partitions are recreated with UTC
-- bounds (plus one for every UTC month those rows fall in, so none is left in
-- the default partition where it would block a later CREATE), and the rows are
-- re-inserted through the parent so each lands in its UTC month. A database
-- whose partitions are all aligned (every session UTC) is left untouched.
--
-- The bound comparison renders the expected values with the same timestamptz
-- output function pg_get_expr uses, so it holds under any session TimeZone or
-- DateStyle. The parent is locked first so no row can be written to a
-- partition between the copy and the DROP.
DO $$
DECLARE
    cols TEXT;
    part RECORD;
    m DATE;
    months DATE[] := '{}';
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext('savings_snapshots_partition_ddl'));
    LOCK TABLE savings_snapshots IN ACCESS EXCLUSIVE MODE;

    SELECT string_agg(quote_ident(attname), ', ' ORDER BY attnum) INTO cols
    FROM pg_attribute
    WHERE attrelid = 'savings_snapshots'::regclass AND attnum > 0 AND NOT attisdropped;

    CREATE TEMP TABLE savings_snapshots_realign
        (LIKE savings_snapshots INCLUDING DEFAULTS) ON COMMIT DROP;

    FOR part IN
        SELECT p.relname, p.month_start
        FROM (
            SELECT c.relname,
                   pg_get_expr(c.relpartbound, c.oid) AS bound,
                   -- CASE, not the WHERE below, guards TO_DATE: the planner
                   -- may evaluate it before filtering.
                   CASE WHEN c.relname ~ '^savings_snapshots_\d{4}_(0[1-9]|1[0-2])$'
                        THEN TO_DATE(RIGHT(c.relname, 7), 'YYYY_MM')
                   END AS month_start
            FROM pg_inherits i
            JOIN pg_class c ON c.oid = i.inhrelid
            WHERE i.inhparent = 'savings_snapshots'::regclass
        ) p
        WHERE p.month_start IS NOT NULL
          AND p.bound IS DISTINCT FROM format(
            'FOR VALUES FROM (%L) TO (%L)',
            p.month_start::timestamp AT TIME ZONE 'UTC',
            (p.month_start + INTERVAL '1 month')::timestamp AT TIME ZONE 'UTC')
    LOOP
        EXECUTE format('INSERT INTO savings_snapshots_realign (%s) SELECT %s FROM %I',
                       cols, cols, part.relname);
        EXECUTE format('DROP TABLE %I', part.relname);
        months := months || part.month_start;
        RAISE NOTICE 'Rebuilding misaligned partition %', part.relname;
    END LOOP;

    IF cardinality(months) = 0 THEN
        RETURN;
    END IF;

    months := months || ARRAY(
        SELECT DISTINCT DATE_TRUNC('month', r.timestamp AT TIME ZONE 'UTC')::date
        FROM savings_snapshots_realign r);

    EXECUTE format(
        'WITH moved AS (
             DELETE FROM savings_snapshots_default d
             WHERE DATE_TRUNC(''month'', d.timestamp AT TIME ZONE ''UTC'')::date = ANY ($1)
             RETURNING %s)
         INSERT INTO savings_snapshots_realign (%s) SELECT %s FROM moved',
        cols, cols, cols) USING months;

    FOR m IN SELECT DISTINCT unnest(months) ORDER BY 1 LOOP
        PERFORM create_savings_snapshot_partition(m);
    END LOOP;

    EXECUTE format('INSERT INTO savings_snapshots (%s) SELECT %s FROM savings_snapshots_realign',
                   cols, cols);
END $$;
