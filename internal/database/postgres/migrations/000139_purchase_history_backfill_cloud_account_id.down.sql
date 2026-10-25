-- Intentionally a no-op. The up migration does not record which rows it
-- filled, so a down cannot tell a backfilled cloud_account_id from one a
-- purchase stored itself, and clearing them all would also detach rows the
-- application stamped. Keeping the backfilled values is harmless: readers
-- match cloud_account_id and the external account_id alike, and each value
-- points at the account that the row's (provider, account_id) already names.
SELECT 1;
