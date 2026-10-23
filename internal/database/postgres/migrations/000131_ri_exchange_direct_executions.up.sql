-- Audit trail for RI exchanges executed directly through the API (issue #82).
--
-- POST /api/ri-exchange/execute (AWS) and POST
-- /api/ri-exchange/azure-instances/exchange (Azure) quote and commit an
-- irreversible exchange in one request. Unlike the scheduled approve/reject
-- flow, which persists every exchange in ri_exchange_history, they wrote no
-- record at all, so an operator could not answer "who exchanged what, when,
-- for how much" after the fact.
--
-- This is a separate table rather than more rows in ri_exchange_history
-- because that table is not a passive log:
--   * account_id is CHECKed to a 12-digit AWS account, so an Azure exchange
--     (subscription GUID) cannot be stored there at all;
--   * its 'processing' and 'completed' rows ARE the scheduled flow's daily
--     spend ledger (GetRIExchangeDailySpend) and its 'processing' rows are
--     swept by the stale-processing reaper, so adding direct executes there
--     would silently change the cap and reaper behavior of the approval flow.
--
-- Lifecycle. The handler inserts the row as 'submitted' before the provider
-- call (and refuses the exchange if it cannot), then finalizes it once:
--   completed  the provider reported success
--   failed     the exchange did not happen: the provider rejected it as a
--              client fault, or the request was refused before reaching the
--              provider (a duplicate submit inside the idempotency window)
--   unknown    the provider call failed with an error that does not prove
--              the exchange was rejected (timeout, 5xx, transport error), so
--              it may have committed; verify in the provider console
-- A row left 'submitted' means the outcome was never recorded (process died
-- or the terminal write failed), which is exactly what an auditor must see.
--
-- No foreign keys: the trail must outlive the user, API key and cloud account
-- it names. executed_by keeps the human-readable identity (email, or the
-- admin API key actor) for when the referenced user row is gone.
CREATE TABLE IF NOT EXISTS ri_exchange_direct_executions (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    provider TEXT NOT NULL CHECK (provider IN ('aws', 'azure')),
    -- cloud_accounts.id the exchange was authorized against; NULL when the
    -- deployment or subscription maps to no registered account.
    cloud_account_id TEXT,
    -- The provider's own account identifier: the Azure subscription ID. NULL
    -- for AWS, which always exchanges in the deployment's own account
    -- (cloud_account_id).
    provider_account_id TEXT,
    -- AWS region. NULL for Azure, whose target locations are in targets.
    region TEXT,
    -- AWS reserved instance IDs, or Azure reservation resource IDs.
    source_ids TEXT[] NOT NULL CHECK (cardinality(source_ids) > 0),
    -- The sources as submitted, with the per-source quantity Azure exchanges
    -- (a partial exchange of a reservation). NULL for AWS, which exchanges the
    -- whole RIs listed in source_ids.
    sources JSONB CHECK (sources IS NULL OR (jsonb_typeof(sources) = 'array' AND jsonb_array_length(sources) > 0)),
    -- The targets as submitted (AWS offering id + count; Azure SKU, location,
    -- term, quantity), always a non-empty JSON array.
    targets JSONB NOT NULL CHECK (jsonb_typeof(targets) = 'array' AND jsonb_array_length(targets) > 0),
    -- The caller's spend cap and its currency.
    max_payment_due NUMERIC NOT NULL,
    max_payment_currency TEXT NOT NULL,
    -- Azure's pre-execute re-quote that cleared the guardrails. NULL for AWS,
    -- which quotes inside the provider call (see net_payable).
    quoted_net_payable NUMERIC,
    -- AWS exchange ID, or Azure exchange session ID.
    provider_reference TEXT,
    status TEXT NOT NULL DEFAULT 'submitted'
        CHECK (status IN ('submitted', 'completed', 'failed', 'unknown')),
    -- Net amount payable reported by the provider; negative is a refund.
    net_payable NUMERIC,
    net_payable_currency TEXT,
    -- Azure's operation status string (e.g. "Succeeded", "PendingPurchases").
    provider_status TEXT,
    error TEXT,
    executed_by_user_id UUID,
    executed_by TEXT NOT NULL CHECK (executed_by <> ''),
    executed_by_api_key_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    -- A row is finalized exactly when it has left 'submitted'.
    CONSTRAINT ri_exchange_direct_executions_completed_at_check
        CHECK ((status = 'submitted') = (completed_at IS NULL))
);

CREATE INDEX IF NOT EXISTS idx_ri_exchange_direct_executions_created
    ON ri_exchange_direct_executions (created_at DESC);

-- Rows whose outcome was never recorded are the ones an operator has to chase.
CREATE INDEX IF NOT EXISTS idx_ri_exchange_direct_executions_unresolved
    ON ri_exchange_direct_executions (created_at)
    WHERE status IN ('submitted', 'unknown');
