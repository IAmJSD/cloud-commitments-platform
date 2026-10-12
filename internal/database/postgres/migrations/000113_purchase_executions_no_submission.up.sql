-- Migration 000113 (platform#118 PR-C1): persisted proof that a failed
-- execution never reached a cloud purchase call.
--
-- purchase_executions.no_submission: TRUE only when the failure happened
-- before any provider call (the beforeSubmissionError paths). Default FALSE
-- (fail closed): every existing row, every reaped row and every
-- outcome-unknown failure has no proof, so the ladder reconciler keeps the
-- tranche counted as in flight.
--
-- Additive and metadata-only (constant default); idempotent.
ALTER TABLE purchase_executions
    ADD COLUMN IF NOT EXISTS no_submission BOOLEAN NOT NULL DEFAULT FALSE;
