# Known Issues: execution reads that skip the legacy cancel actor (#47)

> **Audit status (2026-11-04):** `1 needs triage · 0 resolved`

## LOW: two ramp reads select canceled_by without the legacy cancelled_by fallback

**Files:** `internal/config/store_postgres_ramp_baseline.go` (`ListPlanCommittedTx`), `internal/config/store_postgres_ramp_step.go` (`ListExecutionsForPlanStepTx`)

**Description:** Every other execution projection reads the cancel actor as `COALESCE(canceled_by, cancelled_by)`. These two select `canceled_by` alone. A legacy row whose actor was only ever written to `cancelled_by` (by builds before #1253) comes back with no actor here. Both callers decide by `status`, not by the actor, so no ramp, capacity or purchase decision changes. Only the `CancelledBy` field on the returned rows is empty.

**Why deferred:** Pre-existing, from before #1253, and it doesn't affect any decision. The final step of the rename drains `cancelled_by` into `canceled_by` before dropping the column, and that fixes these rows at the source.

**Issue:** #1747 (follow-up of #47 and #1253)

**Status:** needs triage
