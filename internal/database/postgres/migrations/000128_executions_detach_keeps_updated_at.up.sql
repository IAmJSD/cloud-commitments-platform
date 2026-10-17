-- 000128: keep purchase_executions.updated_at when a row is only detached from
-- a deleted cloud account (issue #265).
--
-- DeleteCloudAccount now detaches the account's terminal executions
-- (cloud_account_id = NULL) so they survive the delete as history instead of
-- tripping migration 000053's ON DELETE RESTRICT FK. The shared
-- update_updated_at_column() trigger would stamp NOW() on every detached row,
-- and updated_at is read as a lifecycle clock: CountExecutionsByPlanAndStatus
-- windows the plan health score's failed/canceled counts on it, and
-- CleanupOldExecutions ages canceled rows by it. Deleting an account would
-- then pull its old failures back into every plan's health window and restart
-- the retention clock on its canceled rows.
--
-- This trigger function keeps OLD.updated_at for an UPDATE whose only change
-- is cloud_account_id going from a value to NULL, and stamps NOW() on every
-- other UPDATE exactly as before. The whole-row comparison runs only inside
-- the detach branch, so ordinary updates pay for one extra NULL check.

CREATE OR REPLACE FUNCTION purchase_executions_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.cloud_account_id IS NOT NULL AND NEW.cloud_account_id IS NULL THEN
        IF (to_jsonb(NEW) - 'cloud_account_id' - 'updated_at')
           = (to_jsonb(OLD) - 'cloud_account_id' - 'updated_at') THEN
            NEW.updated_at = OLD.updated_at;
            RETURN NEW;
        END IF;
    END IF;
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS update_purchase_executions_updated_at ON purchase_executions;
CREATE TRIGGER update_purchase_executions_updated_at
    BEFORE UPDATE ON purchase_executions
    FOR EACH ROW EXECUTE FUNCTION purchase_executions_set_updated_at();
