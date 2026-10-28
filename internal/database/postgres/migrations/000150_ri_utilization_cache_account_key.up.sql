-- Add the account identity to the RI utilization cache key (issue #120).
--
-- The cached payload is one AWS account's Cost Explorer
-- GetReservationUtilization result, but the key was only
-- (region, lookback_days). Once the RI exchange / reshape path reads with
-- per-account credentials, account A's row would be served to account B for
-- the same region and lookback within the TTL. cloud_account_id is the
-- cloud_accounts UUID of the account whose credentials made the call, or ''
-- when the deployment's ambient identity is not registered in cloud_accounts
-- (a primary key column cannot be NULL). It is TEXT with no foreign key: this
-- is a cache, and a row left behind by a deleted account is never read again.
--
-- Existing rows carry no account identity, so they are deleted rather than
-- guessed at; the next read refetches from Cost Explorer. The table is locked
-- first so a writer still running the previous release cannot insert a row
-- between the DELETE and the NOT NULL column add (which would fail the
-- migration). Such a writer's later upserts fail with "no unique or exclusion
-- constraint matching the ON CONFLICT specification"; the cache logs and
-- ignores upsert errors, so it only loses that cache write.
LOCK TABLE ri_utilization_cache IN ACCESS EXCLUSIVE MODE;

DELETE FROM ri_utilization_cache;

ALTER TABLE ri_utilization_cache
    ADD COLUMN IF NOT EXISTS cloud_account_id TEXT NOT NULL;

ALTER TABLE ri_utilization_cache DROP CONSTRAINT IF EXISTS ri_utilization_cache_pkey;
ALTER TABLE ri_utilization_cache
    ADD CONSTRAINT ri_utilization_cache_pkey PRIMARY KEY (cloud_account_id, region, lookback_days);
