-- Configuration audit trail (issue #1493).
--
-- Writes to account_service_overrides (the single PUT and DELETE endpoints and
-- the bulk import) recorded nothing, so nobody could answer "who changed this
-- account's override, when, and from what". Each change now writes one row
-- here, in the same transaction as the change itself:
--
--   account_service_override.upsert  a PUT or an imported row created or
--                                    changed an override (before is NULL for
--                                    a create)
--   account_service_override.delete  a DELETE removed one (after is NULL)
--   account_service_override.import  an applied import; its mode and row
--                                    counts are in details, and every row it
--                                    wrote links back via parent_event_id
--
-- A save that changes nothing, a delete of a missing override and a dry-run
-- import write no rows.
--
-- before / after are the stored override's settings (enabled, term, payment,
-- coverage, ramp_schedule and the include/exclude filters) as JSON.
--
-- No foreign keys to users, API keys or cloud accounts: the trail must outlive
-- what it names (an account delete cascades its overrides away). actor keeps
-- the human-readable identity (email, or the admin API key actor) for when the
-- referenced user row is gone. seq orders events: every event of one
-- transaction shares created_at.
CREATE TABLE IF NOT EXISTS config_audit_events (
    id UUID PRIMARY KEY,
    seq BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
    event_type TEXT NOT NULL CHECK (event_type IN (
        'account_service_override.upsert',
        'account_service_override.delete',
        'account_service_override.import'
    )),
    parent_event_id UUID REFERENCES config_audit_events (id),
    actor TEXT NOT NULL CHECK (actor <> ''),
    actor_user_id UUID,
    actor_api_key_id UUID,
    account_id UUID,
    provider TEXT,
    service TEXT,
    before JSONB,
    after JSONB,
    details JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A per-override event names its override and records a change.
    CONSTRAINT config_audit_events_override_target_check CHECK (
        event_type = 'account_service_override.import'
        OR (account_id IS NOT NULL AND provider IS NOT NULL AND service IS NOT NULL)
    ),
    CONSTRAINT config_audit_events_override_change_check CHECK (
        (event_type <> 'account_service_override.upsert' OR after IS NOT NULL)
        AND (event_type <> 'account_service_override.delete' OR (before IS NOT NULL AND after IS NULL))
    ),
    -- An import is the top of its tree.
    CONSTRAINT config_audit_events_import_parent_check CHECK (
        event_type <> 'account_service_override.import' OR parent_event_id IS NULL
    )
);

CREATE INDEX IF NOT EXISTS idx_config_audit_events_account
    ON config_audit_events (account_id, seq DESC)
    WHERE account_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_config_audit_events_parent
    ON config_audit_events (parent_event_id)
    WHERE parent_event_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_config_audit_events_created
    ON config_audit_events (created_at DESC);
