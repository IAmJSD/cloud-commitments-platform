-- Restore the shared updated_at trigger from migration 000001, which stamps
-- NOW() on every UPDATE, including a detach from a deleted account.
DROP TRIGGER IF EXISTS update_purchase_executions_updated_at ON purchase_executions;
CREATE TRIGGER update_purchase_executions_updated_at
    BEFORE UPDATE ON purchase_executions
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

DROP FUNCTION IF EXISTS purchase_executions_set_updated_at();
