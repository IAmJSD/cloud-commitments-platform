-- Backfill purchase_history.cloud_account_id from (provider, external_id)
-- (platform#22).
--
-- Migration 000011 added cloud_account_id without a backfill, and the
-- direct-execute, ambient and legacy write paths leave it NULL. Readers match
-- both cloud_account_id and the external account_id at query time, so this is
-- convergence, not a correctness fix: point each NULL row at the registered
-- account it already belongs to so later code can rely on the UUID column.
--
-- Matching rules:
--   * Join on (provider, external_id), the cloud_accounts UNIQUE key, never
--     on external_id alone: the same number can be registered under two
--     providers (aws/123 and azure/123), and each row must go only to the
--     account of its own provider.
--   * The row's provider is compared case-insensitively. cloud_accounts.provider
--     is CHECK-constrained to lowercase, and the read-side account predicate
--     already treats a legacy 'AWS' row as aws (issue #24), so this attaches
--     no row that a filter for that account does not already return.
--   * account_id is compared exactly, as the read side does. AWS account
--     numbers, Azure subscription GUIDs and GCP project ids are all matched
--     verbatim; a row whose id differs only in case from the registered one
--     stays NULL rather than being guessed at.
--   * A row with an empty provider (unknown) or an empty account_id stays
--     NULL: it cannot be tied to one (provider, external_id) pair.
--   * A row with no matching account, including one whose account was deleted
--     (ON DELETE SET NULL already cleared it) and never re-registered, stays
--     NULL and is still found through the external-id half of the predicate.
--   * A row that already has a cloud_account_id is never touched.
--
-- Concurrency: golang-migrate runs this file in one transaction, and
-- TestMigrationsAreTransactional forbids committing in batches here, so it is
-- one set-based UPDATE (a single scan of purchase_history, one row per
-- commitment bought). It takes ROW EXCLUSIVE on purchase_history, which does
-- not block reads or inserts; only the rows it rewrites are locked until
-- commit. Under READ COMMITTED a row changed by a concurrent writer is
-- re-checked against the WHERE clause on its committed version, so a writer
-- that sets cloud_account_id first wins and other column updates are not lost.
-- The cloud_accounts rows are locked FOR KEY SHARE so an account cannot be
-- deleted between the join and the foreign-key check (which would abort the
-- migration); a delete already in flight is waited for, and its account is
-- then skipped. Rows inserted after the statement starts are not visible to it
-- and keep whatever their writer stored.
--
-- Idempotent: rerunning it only fills rows that are still NULL and matchable.
UPDATE purchase_history AS ph
SET cloud_account_id = ca.id
FROM (
    SELECT id, provider, external_id
    FROM cloud_accounts
    FOR KEY SHARE
) AS ca
WHERE ph.cloud_account_id IS NULL
  AND ph.account_id <> ''
  AND ph.account_id = ca.external_id
  AND LOWER(ph.provider) = ca.provider;
