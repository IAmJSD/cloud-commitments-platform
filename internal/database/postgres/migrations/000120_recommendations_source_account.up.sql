ALTER TABLE recommendations
    ADD COLUMN IF NOT EXISTS source_account_key TEXT
    GENERATED ALWAYS AS (COALESCE(payload->>'source_account_id', '')) STORED;

CREATE UNIQUE INDEX IF NOT EXISTS recommendations_natural_key_source_idx
    ON recommendations (account_key, provider, service, region, resource_type, engine, term, payment_option, availability_zone, platform, tenancy, source_account_key);
