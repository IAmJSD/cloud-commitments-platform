-- Promote never-started legacy ramps to the retention ledger (platform#631).
--
-- Owner decision 2026-10-17: no backwards compatibility. A plan stamped
-- 'legacy' by 000124's default that is a protected multi-step ramp, has not
-- advanced (current_step = 0, no frozen baseline) and has no surviving
-- purchase_executions row of any status is treated as having no pre-ledger
-- history to distrust, so it becomes 'new' and sizes its first step normally.
-- This is not proof it never bought: retention cleanup could have deleted a
-- completed pre-000124 row of a plan whose step never advanced. That narrow
-- risk is accepted under the owner's decision. Mid-ramp legacy plans and any
-- plan with an execution row are deliberately left legacy.
--
-- guard_ramp_ledger_state forces OLD.ramp_ledger_state = 'legacy' to stay
-- legacy on every UPDATE, so the guard is disabled for this one statement.
-- DISABLE TRIGGER names only that trigger on this table (unlike
-- session_replication_role, it needs no superuser and leaves
-- retain_ramp_ledger_execution and every other trigger live). The migration
-- runs in one transaction, so the guard is re-enabled before commit and a
-- failure rolls both back.
ALTER TABLE purchase_plans DISABLE TRIGGER guard_ramp_ledger_state;

UPDATE purchase_plans p
SET ramp_ledger_state = 'new'
WHERE p.ramp_ledger_state = 'legacy'
  AND ramp_ledger_protected(p.ramp_schedule)
  AND COALESCE((p.ramp_schedule ->> 'current_step')::INT, 0) = 0
  AND p.ramp_baseline IS NULL
  AND NOT EXISTS (SELECT 1 FROM purchase_executions e WHERE e.plan_id = p.id);

ALTER TABLE purchase_plans ENABLE TRIGGER guard_ramp_ledger_state;
