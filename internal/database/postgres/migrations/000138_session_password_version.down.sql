-- Reverse 000138: sessions are no longer bound to the password version, and a
-- password change again relies on deleting them (issue #398).
ALTER TABLE sessions DROP COLUMN IF EXISTS password_version;
