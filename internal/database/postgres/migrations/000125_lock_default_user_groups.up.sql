-- Lock the seeded Standard Users and Read-Only Users groups (issue #168).
--
-- Owner decision 2026-10-16: these default groups are system-managed, so the
-- #1737 guard (ErrSystemManagedGroup) refuses API edits and deletes, like
-- Administrators, Purchaser and RI Exchanger. Locking is by id, so a group an
-- admin already renamed or edited is locked as-is; permissions are untouched.
UPDATE groups
SET system_managed = TRUE
WHERE id IN (
    '00000000-0000-5000-8000-000000000005',  -- Standard Users (000057)
    '00000000-0000-5000-8000-000000000006'   -- Read-Only Users (000057)
);
