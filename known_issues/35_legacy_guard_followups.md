# Known Issues: follow-ups from the legacy-writer guards (#631)

> **Audit status (2026-10-22):** `1 needs triage · 1 resolved`

## LOW: cleanup lambda deletes by expires_at with no status predicate

**Files:** `cmd/cleanup-lambda/main.go` (`deleteExpired`)

**Description:** Unlike `CleanupOldExecutions`, the lambda deletes any row whose `expires_at` lapsed, whatever its status. It cannot reach editor-managed state today: no production code assigns `PurchaseExecution.TTL`, so editor roots carry a NULL `expires_at`, and the `retain_ramp_ledger_execution` trigger (migration 000124) keeps every capacity-holding row of an unfinished ramp plan.

**Why deferred:** Pre-existing and inert; the legacy-writer guards only prove the pruning recheck (`TestPruneRechecksRowAfterCommandCancelsIt`).

**Issue:** #998 (follow-up of #631)

**Status:** Needs triage.

## ~~MEDIUM: a failed-with-submission row blocks deleting a managed plan forever~~ — RESOLVED

**Files:** `internal/config/store_postgres_managed_guard.go` (`requireManagedDeleteTx`), `internal/config/store_postgres_ramp_baseline.go` (`holdsCapacity`)

**Description:** Deleting an editor-managed plan is refused while any execution holds capacity, including a failed row with a submission whose retry later completed, because the retry link is not consulted and failed rows are never pruned. Operators can only disable the plan.

**Why deferred:** Fail closed is intended (unknown outcome); a reviewed settle-or-acknowledge operation is a separate feature, not a flag on delete.

**Issue:** #999 (follow-up of #631)

**Status:** ✔️ Resolved

**Resolved by:** an explicit, audited operator settle, `POST /api/purchases/{id}/settle` (`SettleFailedExecution`, migration 000143). It needs `execute:purchases`, account access and the grant's constraints, a reason and `acknowledge_unknown_outcome: true`, and refuses while the row's retry chain may still buy. The row stays `failed` and keeps counting as possibly bought in the ramp ledger; once settled it can no longer be retried and no longer blocks `requireManagedDeleteTx`. Delete itself gained no flag.
