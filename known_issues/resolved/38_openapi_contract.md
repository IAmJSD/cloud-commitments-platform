# Known Issues: OpenAPI dialect validation

> **Audit status (2026-10-18):** `0 needs triage · 1 resolved`

## LOW: two existing schemas fail OpenAPI 3.0.3 structural validation

**Files:** `internal/api/openapi.yaml`

**Description:** `RecommendationsFreshness.last_collection_partial` uses
`nullable` without an explicit type. `PlanRequest.max_step_commitment` uses
a numeric `exclusiveMinimum`, while the declared 3.0.3 dialect requires a
boolean. The baseline validator reports two errors and 16 warnings. These
errors prevent claiming spec-wide structural validity; they are separate
from the insurance contract documentation.

**Why deferred:** dialect corrections affect unrelated contracts and are
tracked separately to keep the insurance documentation change focused.

**Issue:** [#1075](https://github.com/LeanerCloud/cloud-commitments-platform/issues/1075)

**Status:** Fixed in this change: `last_collection_partial` has an explicit `type: object`, and `max_step_commitment` uses `minimum: 0` with boolean `exclusiveMinimum`. `TestOpenAPIDialect303Schemas` pins both.
