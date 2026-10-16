DROP TRIGGER retain_ramp_ledger_execution ON purchase_executions;
DROP FUNCTION retain_ramp_ledger_execution();
DROP TRIGGER guard_ramp_ledger_state ON purchase_plans;
DROP FUNCTION guard_ramp_ledger_state();
DROP FUNCTION ramp_ledger_shape(JSONB, JSONB);
DROP FUNCTION ramp_ledger_holds(TEXT, BOOLEAN);
DROP FUNCTION ramp_ledger_protected(JSONB);
ALTER TABLE purchase_plans DROP COLUMN ramp_ledger_state;
