-- Migration 000138: bind sessions to the user's password (issue #398).
--
-- A password change deletes the user's sessions in a separate, best-effort
-- step after the new hash commits. A session survived that change when the
-- delete failed, or when a login that had verified the old password inserted
-- its session after the delete ran. A session now records the
-- users.password_version (migration 000103) its login read, and
-- ValidateSession rejects it once the two differ, so the trigger's bump in the
-- password write itself revokes every earlier session.
--
-- Existing sessions take their owner's current version, so the deploy does not
-- sign everyone out. Any that outlived a failed delete before this migration
-- stay valid until they expire (24 hours by default).
ALTER TABLE sessions ADD COLUMN password_version BIGINT NOT NULL DEFAULT 0;

UPDATE sessions s
SET password_version = u.password_version
FROM users u
WHERE u.id = s.user_id;
