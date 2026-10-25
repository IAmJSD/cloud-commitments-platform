-- Migration 000137: let ri_exchange_history.account_id hold any cloud's
-- account identifier (issue #144).
--
-- Since 000009 the column has been
--
--   account_id VARCHAR(20) NOT NULL CHECK (account_id ~ '^\d{12}$')
--
-- which is the exact shape of an AWS account ID and nothing else. An Azure
-- subscription ID is a 36-character GUID; a GCP project ID is 6 to 30
-- lowercase letters, digits and hyphens. There are two independent barriers
-- and BOTH have to go:
--
--   1. VARCHAR(20) rejects a 36-character subscription GUID (SQLSTATE 22001).
--   2. The regex CHECK rejects every non-AWS value, including a short GCP
--      project ID that fits in 20 characters, and would keep rejecting an
--      Azure GUID even at VARCHAR(255) (SQLSTATE 23514).
--
-- A migration that only widened the column would pass a width test and still
-- reject every Azure and GCP row. This one widens the column to
-- VARCHAR(255), matching cloud_accounts.external_id,
-- savings_snapshots.account_id (000067) and purchase_history.account_id
-- (000095), and drops the AWS-only CHECK.
--
-- No replacement CHECK: AWS account IDs, Azure subscription GUIDs and GCP
-- project IDs share no common shape, and the table has no provider column to
-- key a provider-aware constraint on. The shape is validated in Go at the
-- write boundary instead, where the provider is known: the only writer today
-- is the AWS exchange store adapter (internal/server/handler_ri_exchange.go),
-- which refuses anything but a 12-digit AWS account ID, so AWS rows keep
-- exactly the guarantee this CHECK gave them -- including refusing the
-- "unknown" placeholder resolveAccountID returns when STS fails. This matches
-- purchase_history.account_id, which has never had a CHECK.
--
-- Today no non-AWS value can reach this column (the Azure exchange path writes
-- no ri_exchange_history row), so nothing is broken yet; this lands the schema
-- change before a multi-cloud writer does, not after its first lost row.
--
-- ------------------------------------------------------------------
-- Guards
-- ------------------------------------------------------------------
-- Correct on a fresh database, an already-deployed one, a re-run under the
-- auto-heal path, and a partially fixed one (widened but still CHECKed, or
-- unCHECKed but still narrow): each half is conditioned on the observed
-- catalog state, because IF NOT EXISTS cannot repair a wrong column type or
-- an existing constraint.
--
-- The table is resolved through 'ri_exchange_history'::regclass, which follows
-- search_path exactly like the bare ALTER TABLE statements below do (see
-- 000095 for why that matters). The column's absence is a hard failure, and
-- both halves are re-checked from the catalog afterwards, so this migration
-- can never be recorded as applied without having done the work.
--
-- The CHECK is found by catalog, not by name: the inline constraint in 000009
-- was auto-named ri_exchange_history_account_id_check, but a database restored
-- or hand-repaired from a dump may carry another name. Only single-column CHECKs
-- on account_id that contain the 12-digit regex are dropped.
--
-- ------------------------------------------------------------------
-- Concurrency
-- ------------------------------------------------------------------
-- golang-migrate serializes its own runs with an advisory lock, but the file is
-- also re-run outside it (auto-heal, an operator replaying it by hand). The
-- table lock is taken FIRST, before any catalog read: otherwise a second run
-- that started while the first was still uncommitted would read the
-- pre-commit catalog, see a CHECK the first run has already dropped, block on
-- that run's lock and then fail with "constraint does not exist" once it
-- commits. Under READ COMMITTED every statement in the DO block takes a fresh
-- snapshot, so once the lock is granted all reads below see the committed
-- state. The lock is held to the end of the migration's transaction.
--
-- Cost: widening a varchar is binary-coercible, so Postgres skips the table
-- rewrite and the rebuild of idx_ri_exchange_history_account (000009), and
-- dropping a CHECK is catalog-only. ACCESS EXCLUSIVE is held only for that.

DO $$
DECLARE
    typmod    INTEGER;
    col       SMALLINT;
    con       RECORD;
    remaining INTEGER;
BEGIN
    LOCK TABLE ri_exchange_history IN ACCESS EXCLUSIVE MODE;

    SELECT a.atttypmod, a.attnum INTO typmod, col
      FROM pg_attribute a
     WHERE a.attrelid = 'ri_exchange_history'::regclass
       AND a.attname  = 'account_id'
       AND NOT a.attisdropped;

    IF col IS NULL THEN
        RAISE EXCEPTION
            'migration 000137: column ri_exchange_history.account_id does not exist';
    END IF;

    -- atttypmod is n + 4 for VARCHAR(n); -1 is unbounded varchar, which is
    -- already wider than VARCHAR(255) and must not be narrowed.
    IF typmod <> -1 AND typmod - 4 < 255 THEN
        ALTER TABLE ri_exchange_history
            ALTER COLUMN account_id TYPE VARCHAR(255);
    END IF;

    FOR con IN
        SELECT c.conname
          FROM pg_constraint c
         WHERE c.conrelid = 'ri_exchange_history'::regclass
           AND c.contype  = 'c'
           AND c.conkey   = ARRAY[col]
           AND position('^\d{12}$' IN pg_get_constraintdef(c.oid)) > 0
    LOOP
        EXECUTE format('ALTER TABLE ri_exchange_history DROP CONSTRAINT %I', con.conname);
    END LOOP;

    -- Post-checks, read back from the catalog.
    SELECT a.atttypmod INTO typmod
      FROM pg_attribute a
     WHERE a.attrelid = 'ri_exchange_history'::regclass
       AND a.attname  = 'account_id'
       AND NOT a.attisdropped;

    IF typmod <> -1 AND typmod - 4 < 255 THEN
        RAISE EXCEPTION
            'migration 000137 failed to widen ri_exchange_history.account_id: still VARCHAR(%)',
            typmod - 4;
    END IF;

    SELECT count(*) INTO remaining
      FROM pg_constraint c
     WHERE c.conrelid = 'ri_exchange_history'::regclass
       AND c.contype  = 'c'
       AND c.conkey   = ARRAY[col]
       AND position('^\d{12}$' IN pg_get_constraintdef(c.oid)) > 0;

    IF remaining > 0 THEN
        RAISE EXCEPTION
            'migration 000137 failed to drop the AWS-only CHECK on ri_exchange_history.account_id';
    END IF;
END $$;
