# Known Issues: plan step failure is not atomic with its stamp and suppression release

> **Audit status (2026-10-20):** `0 needs triage · 1 resolved`

## LOW: crash between the failed CAS and the stamp tx strands the row

**Files:** `internal/purchase/notifications.go` (`failExistingPlanStep`),
`internal/config/store_postgres.go` (`TransitionExecutionStatusPinned`),
`internal/purchase/plan_step_attach.go` (`claimFromRows`, `failStep`)

**Description:** `failExistingPlanStep` first flips the row to `failed` with a
pinned compare-and-set, then saves the `Error` and `NoSubmission` stamp and
deletes the row's suppressions in a second transaction. The CAS cannot join a
caller transaction, so a crash or DB error between the two leaves the row
`failed` with no `Error`, no `NoSubmission`, and its suppressions still in
place until their grace period expires. The `Error`/`NoSubmission` half of the
gap predates the suppression release; the release makes it slightly wider.

**Why it fails closed:** `adoptableRootRow` only adopts pending/notified roots,
so a failed row is skipped on every later tick and the step never gets a second
root. Nothing is bought twice. The cost is a step that stays failed without a
recorded reason and recommendations hidden until the suppressions expire.

**Fix direction:** add a Tx variant of `TransitionExecutionStatusPinned` so the
CAS, the stamp and `DeleteSuppressionsByExecutionTx` commit together.

**Why deferred:** out of scope for the retention-ledger runtime change (#905);
the window is a crash between two statements and the failure mode is safe.

**Status:** Resolved in #1052. `TransitionExecutionStatusPinnedTx` runs the
pinned CAS in a caller transaction, and `failExistingPlanStep` now commits the
CAS, the `Error`/`NoSubmission` stamp and the suppression release together. An
error before commit leaves the row pending for the next tick. Real-Postgres
integration tests inject an error at the release and race the fail against an
approval, a cancel and a second tick.
