-- Intentionally a no-op.
--
-- The up migration cleared a flag that carried no information on revoked
-- rows (revoked_at decides whether a purchase is revoked; the flag only
-- matters while revoked_at is NULL). Which rows held it is not recorded, and
-- setting it back on every revoked row would put rows into the in-flight
-- index that the application never flagged.
SELECT 1;
