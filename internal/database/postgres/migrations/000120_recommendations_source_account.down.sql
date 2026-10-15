DO $$
BEGIN
    IF to_regclass('recommendations_natural_key_az_tp_idx') IS NULL THEN
        RAISE EXCEPTION 'cannot remove source account key without legacy unique index';
    END IF;
END $$;

DROP INDEX IF EXISTS recommendations_natural_key_source_idx;
ALTER TABLE recommendations DROP COLUMN IF EXISTS source_account_key;
