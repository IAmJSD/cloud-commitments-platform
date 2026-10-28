# Scheduled server sweeps on Cloud Run (#712).
#
# Each enabled sweep is a Cloud Scheduler job that POSTs to
# /api/scheduled/<task>, authenticated exactly like the recommendations job in
# main.tf: an OIDC token signed by the scheduler SA (whose unique_id is already
# in SCHEDULED_TASK_OIDC_SUBJECTS and which already holds roles/run.invoker),
# with the audience pinned to the receiving endpoint. Each audience is added to
# SCHEDULED_TASK_OIDC_AUDIENCE through local.sweep_audiences, so the token and
# the validator allowlist match by construction.
#
# enable_scheduled_tasks is the master switch: with it off, no sweep job is
# created whatever the per-task flags say, so a deployment that switched the
# scheduler off does not gain jobs (or purchases) on upgrade.

locals {
  # Keyed by server task name. `job` is the Cloud Scheduler job name suffix.
  # Default-on tasks match the other platforms, including
  # fire_scheduled_purchases; process_scheduled_purchases,
  # send_notifications and cleanup are opt-in (default off, as decided for
  # #712).
  sweep_tasks = {
    reap_stuck_purchases        = { job = "reap", enabled = var.enable_reap_stuck_purchases_schedule, schedule = var.reap_stuck_purchases_schedule }
    analytics_collect           = { job = "analytics", enabled = var.enable_analytics_collect_schedule, schedule = var.analytics_collect_schedule }
    fire_scheduled_purchases    = { job = "fire", enabled = var.enable_fire_scheduled_purchases_schedule, schedule = var.fire_scheduled_purchases_schedule }
    process_scheduled_purchases = { job = "process", enabled = var.enable_process_scheduled_purchases_schedule, schedule = var.process_scheduled_purchases_schedule }
    send_notifications          = { job = "notify", enabled = var.enable_send_notifications_schedule, schedule = var.send_notifications_schedule }
    finalize_revocations        = { job = "finalize", enabled = var.enable_finalize_revocations_schedule, schedule = var.finalize_revocations_schedule }
    cleanup                     = { job = "cleanup", enabled = var.enable_cleanup_schedule, schedule = var.cleanup_schedule }
  }
  active_sweeps = var.enable_scheduled_tasks ? { for k, v in local.sweep_tasks : k => v if v.enabled } : {}

  # Same shape as scheduled_task_recommendations_audience in main.tf (the
  # service name, not the Cloud Run URL, to avoid a dependency cycle).
  sweep_audiences = { for k, v in local.active_sweeps : k => "${var.service_name}/api/scheduled/${k}" }
}

resource "google_cloud_scheduler_job" "sweep" {
  for_each = local.active_sweeps

  name             = "${var.service_name}-${each.value.job}"
  description      = "Trigger the ${each.key} scheduled task"
  schedule         = each.value.schedule
  time_zone        = "UTC"
  attempt_deadline = "320s"
  project          = var.project_id
  region           = var.region

  # Same retries as the other jobs: a re-run is safe under the server's
  # per-task advisory lock and row CAS.
  retry_config {
    retry_count = 3
  }

  http_target {
    http_method = "POST"
    uri         = "${google_cloud_run_v2_service.main.uri}/api/scheduled/${each.key}"

    oidc_token {
      service_account_email = google_service_account.scheduler[0].email
      audience              = local.sweep_audiences[each.key]
    }
  }

  # The scheduler SA must be allowed to invoke the service before the first
  # run, or that run is a 403 when allow_unauthenticated is false.
  depends_on = [google_cloud_run_service_iam_member.scheduler_invoker]
}
