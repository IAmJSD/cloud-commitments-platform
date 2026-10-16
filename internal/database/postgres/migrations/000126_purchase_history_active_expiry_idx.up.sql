-- Index the commitment expiry so GetActivePurchaseHistory can skip expired
-- history (issue #33).
--
-- The active filter used to be timestamp + make_interval(hours => term * 8760)
-- >= $1. That expression spans two columns and timestamptz + interval is only
-- STABLE, so no index could serve it and Postgres evaluated it against every
-- row ever recorded on each analytics cycle, dashboard load and inventory call.
--
-- This is the same expiry as Unix epoch seconds, written with IMMUTABLE parts
-- only (UTC wall time, epoch, numeric arithmetic). For an hours-only interval
-- it is exact, so the active set is unchanged. numeric cannot overflow, so a
-- row with any term value still inserts. The expression must stay identical to
-- activeExpiryEpochExpr in internal/config/store_postgres.go.
--
-- The index is not partial on purpose: Postgres ignores the statistics of a
-- partial index's expression, and without them it estimates a third of the
-- table as active and can keep the full scan even when few rows are active.
CREATE INDEX IF NOT EXISTS idx_purchase_history_active_expiry
    ON purchase_history ((extract(epoch FROM (timestamp AT TIME ZONE 'UTC')) + term::numeric * 31536000));
