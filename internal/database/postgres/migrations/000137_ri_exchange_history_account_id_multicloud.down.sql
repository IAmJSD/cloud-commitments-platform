-- Reverse 000137: narrow ri_exchange_history.account_id back to VARCHAR(20)
-- and restore CHECK (account_id ~ '^\d{12}$').
--
-- Lossy by nature, so it FAILS LOUDLY instead of truncating or deleting. Once
-- 000137 is applied a row may carry an Azure subscription GUID or a GCP project
-- ID; neither can satisfy the restored CHECK, and the GUID cannot fit
-- VARCHAR(20). An exchange-history row is the audit record of money already
-- moved, so the rollback refuses while any account_id is not a 12-digit AWS
-- account ID, names the count, and leaves the decision to the operator.
--
-- Reverse exactly what the up did, nothing more: narrow only from VARCHAR(255)
-- (atttypmod 259), which is what the up produces; a wider or unbounded column
-- was not created by 000137 and is left alone. Re-add the CHECK only when no
-- equivalent one is present, under the name 000009 gave it.
--
-- The table lock is taken first so no row can be inserted between the count
-- and the ALTERs (the same reasoning as the up migration's lock). Unlike the
-- up, narrowing a varchar is NOT binary-coercible: it rewrites the table and
-- rebuilds idx_ri_exchange_history_account, and ADD CONSTRAINT scans every row.
--
-- Recovering from a refused rollback: golang-migrate marks the version dirty
-- before running this file and maybeAutoHealDirty will not clear it. This file
-- raises before changing the schema, so the database still matches 000137.
-- Set CUDLY_FORCE_MIGRATION_VERSION=137 (the CURRENT version), redeploy, remove
-- the env var, then archive or reconcile the non-AWS rows if the rollback is
-- still wanted.

DO $$
DECLARE
    typmod   INTEGER;
    col      SMALLINT;
    non_aws  BIGINT;
    existing INTEGER;
BEGIN
    LOCK TABLE ri_exchange_history IN ACCESS EXCLUSIVE MODE;

    SELECT count(*) INTO non_aws
      FROM ri_exchange_history
     WHERE account_id !~ '^\d{12}$';

    IF non_aws > 0 THEN
        RAISE EXCEPTION
            'refusing to restore the AWS-only account_id constraint on ri_exchange_history: '
            '% row(s) hold an account ID that is not a 12-digit AWS account ID '
            '(an Azure subscription GUID or GCP project ID). Narrowing would drop '
            'or truncate exchange audit rows. Archive or reconcile those rows before '
            'rolling back migration 000137.',
            non_aws;
    END IF;

    SELECT a.atttypmod, a.attnum INTO typmod, col
      FROM pg_attribute a
     WHERE a.attrelid = 'ri_exchange_history'::regclass
       AND a.attname  = 'account_id'
       AND NOT a.attisdropped;

    -- 259 == VARCHAR(255) (atttypmod is n + 4).
    IF typmod = 259 THEN
        ALTER TABLE ri_exchange_history
            ALTER COLUMN account_id TYPE VARCHAR(20);
    END IF;

    SELECT count(*) INTO existing
      FROM pg_constraint c
     WHERE c.conrelid = 'ri_exchange_history'::regclass
       AND c.contype  = 'c'
       AND c.conkey   = ARRAY[col]
       AND position('^\d{12}$' IN pg_get_constraintdef(c.oid)) > 0;

    IF existing = 0 THEN
        ALTER TABLE ri_exchange_history
            ADD CONSTRAINT ri_exchange_history_account_id_check
            CHECK (account_id ~ '^\d{12}$');
    END IF;
END $$;
