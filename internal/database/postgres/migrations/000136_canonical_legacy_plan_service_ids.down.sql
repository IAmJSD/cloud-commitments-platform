-- Intentionally a no-op: the up migration keeps each entry's key, and an API
-- edit since #1123 also stores the canonical service under a legacy key, so a
-- rewritten entry cannot be told apart from an edited one. Restoring the old
-- ids would also make those plans match no recommendations again.
SELECT 1;
