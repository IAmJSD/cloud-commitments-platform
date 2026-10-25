# Known Issues: follow-ups from the legacy-writer guards (#631)

> **Audit status (2026-10-20):** `1 needs triage · 1 resolved`

## ~~LOW: cleanup lambda deletes by expires_at with no status predicate~~ — RESOLVED

**Files:** `cmd/cleanup-lambda/main.go` (`deleteExpired`)

**Description:** Unlike `CleanupOldExecutions`, the lambda deletes any row whose `expires_at` lapsed, whatever its status. It cannot reach editor-managed state today: no production code assigns `PurchaseExecution.TTL`, so editor roots carry a NULL `expires_at`, and the `retain_ramp_ledger_execution` trigger (migration 000124) keeps every capacity-holding row of an unfinished ramp plan.

**Why deferred:** Pre-existing and inert; the legacy-writer guards only prove the pruning recheck (`TestPruneRechecksRowAfterCommandCancelsIt`).

**Issue:** #998 (follow-up of #631)

**Status:** ✔️ Resolved

**Resolved by:** `deleteExpired` and `dryRunCount` now share `expiredExecutionsWhere`: a lapsed `expires_at` deletes a row only when its status is in `TerminalExecutionStatuses` and not in `HealthScoredExecutionStatuses`, so live rows survive and failed or canceled rows stay with `CleanupOldExecutions`. Covered by the real-Postgres tests in `cmd/cleanup-lambda/main_integration_test.go`, including a writer that turns a settled row live while the DELETE waits for its lock.

## MEDIUM: a failed-with-submission row blocks deleting a managed plan forever

**Files:** `internal/config/store_postgres_managed_guard.go` (`requireManagedDeleteTx`), `internal/config/store_postgres_ramp_baseline.go` (`holdsCapacity`)

**Description:** Deleting an editor-managed plan is refused while any execution holds capacity, including a failed row with a submission whose retry later completed, because the retry link is not consulted and failed rows are never pruned. Operators can only disable the plan.

**Why deferred:** Fail closed is intended (unknown outcome); a reviewed settle-or-acknowledge operation is a separate feature, not a flag on delete.

**Issue:** #999 (follow-up of #631)

**Status:** Needs triage.
