# Known Issues: Archera provider initialization error logs

> **Audit status (2026-10-18):** `0 needs triage · 1 resolved`

## HIGH: Raw resolver diagnostics could disclose credential details

**Files:** `internal/archera/provider.go`, `internal/archera/provider_log_test.go`

**Description:** `Provider.Client` returned the fixed `ErrKeyUnavailable` but logged raw resolver and constructor errors. A synthetic resolver failure demonstrated key, backend and secret-reference text reaching the warning. No real credential disclosure was observed. The pinned constructor already returns fixed diagnostics that do not contain the key.

**Why deferred:** Not deferred; isolated from the separate context-wait change in #1076.

**Issue:** [#1077](https://github.com/LeanerCloud/cloud-commitments-platform/issues/1077)

**Status:** Fixed in this change with constant stage-specific warnings. Real-provider captured-log regressions cover resolver failures and actual constructor rejections, preserve caller errors, and check zero HTTP calls, retry success and cached-client identity. The tests fail on the parent because resolver sentinels and constructor diagnostics reach logs. Independent review and publication remain subject to the PR gates.
