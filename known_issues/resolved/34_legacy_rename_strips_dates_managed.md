# Known Issues: legacy plan rename strips editor markers

> **Audit status (2026-10-16):** `0 needs triage · 1 resolved`

## HIGH: PUT rename dropped dates_managed and allocation_mode

**Files:** `internal/api/handler_plans.go` (`preserveRampProgress`), `internal/config/store_postgres.go`

**Description:** A legacy `PUT /plans/{id}` rebuilds the ramp schedule from the request, and `preserveRampProgress` carried over only the step, start date and next date. A bare rename of an editor-managed plan therefore wrote the plan back without `dates_managed` and `allocation_mode`, returning it to the interval formula (#631).

**Why deferred:** Not deferred; found while planning the legacy-writer guards and fixed in the same change.

**Issue:** #631

**Status:** Resolved in the G1a guard PR. `preserveRampProgress` keeps both markers, and `UpdatePurchasePlanTx` refuses legacy writes that change editor-owned fields of a managed plan. Regression test: `TestUpdatePlanRenameKeepsManagedMarkers`.
