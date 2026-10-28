# purchase_executions retention — verify CleanupOldExecutions is scheduled

**Surfaced during:** 2026-04-23 follow-up audit — Failed + Expired rows now persist and surface in History
**Related commits:** `32f9e4ffc` (pending-in-history), `b44283746` (failed + expired states)
**Status:** Partially resolved. The cleanup task exists (verified 2026-04-25) but retention is unscheduled by default on AWS and GCP (opt-in there); Azure Container Apps schedules it.

> **Amendment (#712):** the AWS trigger claimed below (`cleanup-lambda/main.tf:98`)
> is false: no environment consumes the `cleanup-lambda` module, so nothing
> schedules cleanup on AWS. The same holds for the Terraform behind the
> "matches what the cleanup-lambda Terraform actually configures" sentence
> under "Original problem". Only Azure Container Apps schedules cleanup today. Retention
> scheduling is tracked by #712; retention is scheduled via the server `cleanup` task on AWS Lambda, AWS Fargate, GCP Cloud Run, GKE and AKS when opted in.

## Resolution

`internal/server/handler.go::handleCleanupExpiredRecords` (line 272) calls
`CleanupOldExecutions(ctx, 30)` with a 30-day retention. Wired as the
`TaskCleanupExpiredRecords` scheduled task type and triggered by:

- **AWS:** ~~EventBridge rule `cleanup_schedule` in
  `terraform/modules/compute/aws/cleanup-lambda/main.tf:98`~~ (unconsumed module, see amendment)
- **AWS Lambda (opt-in):** the `enable_cleanup_schedule` / `cleanup_schedule`
  sweep `<stack>-sweep-cleanup` runs the server cleanup task.
- **AWS Fargate (opt-in):** the same `enable_cleanup_schedule` /
  `cleanup_schedule` variables add a `<stack>-fargate-sweep-cleanup` rule
  that runs `./cudly --task cleanup` as a one-off ECS task.
- ~~**CloudFormation (opt-in):** the `EnableCleanupSchedule` /
  `CleanupSchedule` stack parameters add a `<stack>-Cleanup` rule that runs
  the server cleanup task.~~ (the CloudFormation install stack was removed in
  #1432; use the Terraform AWS install)
- **Azure Container Apps:** a Logic App calls `/api/scheduled/cleanup` daily
  when `enable_scheduled_tasks` is true. (The `azure/cleanup-function` module
  is not consumed by any environment.)
- **GCP Cloud Run and GKE (opt-in):** `enable_cleanup_schedule` /
  `cleanup_schedule` add a `<name>-cleanup` Cloud Scheduler job that calls
  `/api/scheduled/cleanup` (#712), when the module's `enable_scheduled_tasks`
  is true.
- **AKS (opt-in):** `enable_cleanup_schedule` / `cleanup_schedule` on the AKS
  module add a `<project>-task-cleanup` CronJob that runs the server cleanup
  task (#893), when `deploy_kubernetes_resources` is true.

The `CleanupOldExecutions` Postgres implementation in
`internal/config/store_postgres.go` runs the expected
`DELETE FROM purchase_executions WHERE …`, so the table is bounded
only where that task is scheduled (Azure Container Apps by default; AWS Lambda, AWS Fargate, GCP and AKS when opted in, see amendment).

## Original problem (historical context)

Before the merge of non-completed executions into History
(`32f9e4ffc` and `b44283746`), only `completed` purchases surfaced via
`purchase_history` — which is itself pruned in its own SQL. After those
commits, `purchase_executions` rows in `pending` / `notified` / `failed`
/ `expired` states also render on every `/api/history` request, and
those rows would have stuck around indefinitely without a cleanup
caller.

At the time this issue was filed, `grep -rn CleanupOldExecutions
internal/` showed only the interface + implementation + tests; no
caller. The concern was that, left unbounded, a high-volume tenant with
one failed purchase per day (common when FROM_EMAIL is misconfigured —
see `33pz7pom…` before `b3e17719b`) would accumulate a thousand failed
rows a year, eventually amplifying writes during `expireIfStale`
transitions on every History page load.

The wiring discovered above (handleCleanupExpiredRecords →
TaskCleanupExpiredRecords → cleanup_schedule) closes that gap. The
30-day retention is shorter than the 90-day window originally proposed
but matches what the cleanup-lambda Terraform actually configures, and
is sufficient for "what failed last week" debugging without ballooning
the table.

## Future improvements (not blocking)

- **Metrics** — emit a CloudWatch metric for the count of non-completed
  executions so we can alarm before the table becomes a performance
  issue, not after. Tracked separately if it matters; not part of this
  issue's resolution.
