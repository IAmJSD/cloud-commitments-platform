-- 000135: bind display-name entries in groups.allowed_accounts to account IDs
-- (issue #1034).
--
-- Account scopes now match allowed_accounts entries by cloud account ID only
-- (auth.AccountScope.Allows). A display-name entry is not bound to any
-- account: once the account it was meant for is deleted or renamed, it would
-- grant whichever account is later created or renamed to that name. Group
-- writes now resolve names to IDs at grant time; this migration does the same
-- for the entries already stored.
--
-- An entry is rewritten to an account's ID only when ALL of these hold:
--   * it is not "*" (the wildcard) and not empty;
--   * it is not already an account ID or an account's external ID (those
--     are kept verbatim, as the write path keeps them);
--   * exactly one cloud account, of any provider, enabled or not, carries
--     it as its display name (a shared name cannot say which one was meant);
--   * that account already existed when the group was last written
--     (created_at <= groups.updated_at). An account registered after the
--     grant cannot be the account the grant was made for, which is exactly
--     the delete-then-recreate case of the issue.
--
-- Every other entry is left as it is. It no longer matches any account, so
-- the group loses that access (fail closed) until an administrator grants the
-- intended account by ID. Such entries can be listed with:
--
--   SELECT g.name, e.entry
--   FROM groups g, unnest(g.allowed_accounts) AS e(entry)
--   WHERE e.entry <> '*'
--     AND NOT EXISTS (SELECT 1 FROM cloud_accounts ca
--                     WHERE ca.id::text = e.entry OR ca.external_id = e.entry);
--
-- Order is preserved. Groups with a NULL or empty list are untouched, and an
-- entry is only ever replaced, never dropped, so a restricted list can never
-- become empty (which would mean unrestricted).
--
-- update_groups_updated_at would stamp every rewritten group with NOW(). That
-- is not a grant decision, and it would weaken the created_at guard above if
-- this migration is ever re-applied (down is a no-op), letting an entry left
-- unresolved the first time bind to an account registered after the real
-- grant. So the trigger is disabled for this one statement. DISABLE TRIGGER
-- names only that trigger and needs no superuser; the migration runs in one
-- transaction, so it is re-enabled before commit and a failure rolls both back.
ALTER TABLE groups DISABLE TRIGGER update_groups_updated_at;

UPDATE groups g
SET allowed_accounts = bound.entries
FROM (
    SELECT src.id,
           array_agg(COALESCE(match.account_id, e.entry) ORDER BY e.ord) AS entries
    FROM groups src
    CROSS JOIN LATERAL unnest(src.allowed_accounts) WITH ORDINALITY AS e(entry, ord)
    LEFT JOIN LATERAL (
        SELECT min(ca.id::text) AS account_id
        FROM cloud_accounts ca
        WHERE ca.name = e.entry
        HAVING count(*) = 1
           AND bool_and(ca.created_at <= src.updated_at)
    ) match ON e.entry <> '*'
           AND e.entry <> ''
           AND NOT EXISTS (
               SELECT 1 FROM cloud_accounts known
               WHERE known.id::text = e.entry OR known.external_id = e.entry
           )
    GROUP BY src.id
) bound
WHERE g.id = bound.id
  AND g.allowed_accounts IS DISTINCT FROM bound.entries;

ALTER TABLE groups ENABLE TRIGGER update_groups_updated_at;
