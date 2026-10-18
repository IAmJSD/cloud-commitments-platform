-- Migration 000130 (issue #233): marks executions the scheduler minted with no
-- human initiator (a NULL creator alone also covers API-key, deleted-user and
-- legacy rows). Insert-only: never rewritten by the upsert.
ALTER TABLE purchase_executions
    ADD COLUMN IF NOT EXISTS created_by_system BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE purchase_executions
    ADD CONSTRAINT purchase_executions_created_by_system_no_creator
    CHECK (NOT created_by_system OR created_by_user_id IS NULL);
