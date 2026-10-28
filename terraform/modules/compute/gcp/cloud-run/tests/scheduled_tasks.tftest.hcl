# Scheduled server sweeps on Cloud Run (#712). Every provider is mocked;
# nothing here reaches GCP. Each sweep must be a Cloud Scheduler job calling
# /api/scheduled/<task> with an OIDC token whose audience the service's
# SCHEDULED_TASK_OIDC_AUDIENCE allowlist accepts, and the money and retention
# sweeps must stay off unless asked for.
mock_provider "google" {}

variables {
  project_id                  = "cudly-test-project"
  service_name                = "cudly-test"
  environment                 = "dev"
  region                      = "us-central1"
  image_uri                   = "us-central1-docker.pkg.dev/cudly-test-project/cudly/cudly:test"
  database_host               = "10.0.0.5"
  database_name               = "cudly"
  database_username           = "cudly"
  database_password_secret_id = "cudly-test-db-password"
  admin_email                 = "admin@example.com"
}

run "defaults_schedule_only_the_default_on_sweeps" {
  command = apply

  assert {
    condition     = toset(keys(google_cloud_scheduler_job.sweep)) == toset(["analytics_collect", "finalize_revocations", "reap_stuck_purchases"])
    error_message = "By default only reap, analytics and finalize run; fire, process, send_notifications and cleanup are opt-in."
  }

  assert {
    condition = alltrue([for k, j in google_cloud_scheduler_job.sweep :
      endswith(j.http_target[0].uri, "/api/scheduled/${k}") && j.http_target[0].http_method == "POST"
    ])
    error_message = "Each sweep job must POST to /api/scheduled/<its task>."
  }

  assert {
    condition = alltrue([for k, j in google_cloud_scheduler_job.sweep :
      j.http_target[0].oidc_token[0].service_account_email == google_service_account.scheduler[0].email &&
      j.http_target[0].oidc_token[0].audience == "cudly-test/api/scheduled/${k}"
    ])
    error_message = "Each sweep job must sign with the scheduler SA (already in the subject allowlist) and pin the audience to its endpoint."
  }

  assert {
    condition = alltrue([for k, j in google_cloud_scheduler_job.sweep :
      contains(split(",", one([for e in google_cloud_run_v2_service.main.template[0].containers[0].env : e.value if e.name == "SCHEDULED_TASK_OIDC_AUDIENCE"])), j.http_target[0].oidc_token[0].audience)
    ])
    error_message = "SCHEDULED_TASK_OIDC_AUDIENCE must accept every sweep job's audience, or the validator 401s the call."
  }

  assert {
    condition     = contains(split(",", one([for e in google_cloud_run_v2_service.main.template[0].containers[0].env : e.value if e.name == "SCHEDULED_TASK_OIDC_AUDIENCE"])), "cudly-test/api/scheduled/collect_recommendations")
    error_message = "The recommendations audience must stay in the allowlist."
  }

  assert {
    condition     = google_cloud_scheduler_job.sweep["reap_stuck_purchases"].schedule == "*/5 * * * *" && google_cloud_scheduler_job.sweep["reap_stuck_purchases"].name == "cudly-test-reap"
    error_message = "The reap sweep must use its schedule variable and job name."
  }
}

run "opt_in_sweeps_follow_their_flags" {
  command = apply

  variables {
    enable_fire_scheduled_purchases_schedule    = true
    enable_process_scheduled_purchases_schedule = true
    enable_send_notifications_schedule          = true
    enable_cleanup_schedule                     = true
    enable_analytics_collect_schedule           = false
    fire_scheduled_purchases_schedule           = "*/10 * * * *"
  }

  assert {
    condition = toset(keys(google_cloud_scheduler_job.sweep)) == toset([
      "cleanup", "finalize_revocations", "fire_scheduled_purchases", "process_scheduled_purchases",
      "reap_stuck_purchases", "send_notifications",
    ])
    error_message = "Each per-task flag must switch its job on or off independently."
  }

  assert {
    condition     = google_cloud_scheduler_job.sweep["fire_scheduled_purchases"].schedule == "*/10 * * * *"
    error_message = "fire_scheduled_purchases_schedule must reach the job."
  }

  assert {
    condition = alltrue([for k, j in google_cloud_scheduler_job.sweep :
      contains(split(",", one([for e in google_cloud_run_v2_service.main.template[0].containers[0].env : e.value if e.name == "SCHEDULED_TASK_OIDC_AUDIENCE"])), j.http_target[0].oidc_token[0].audience)
    ])
    error_message = "SCHEDULED_TASK_OIDC_AUDIENCE must accept every enabled sweep's audience."
  }

  assert {
    condition     = !contains(split(",", one([for e in google_cloud_run_v2_service.main.template[0].containers[0].env : e.value if e.name == "SCHEDULED_TASK_OIDC_AUDIENCE"])), "cudly-test/api/scheduled/analytics_collect")
    error_message = "A disabled sweep's audience must not be accepted."
  }
}

run "scheduler_off_creates_no_sweeps" {
  command = apply

  variables {
    enable_scheduled_tasks                   = false
    enable_ri_exchange_schedule              = true
    enable_fire_scheduled_purchases_schedule = true
  }

  assert {
    condition     = length(google_cloud_scheduler_job.sweep) == 0
    error_message = "enable_scheduled_tasks is the master switch: with it off no sweep job may exist, even with a per-task flag on."
  }

  assert {
    condition     = one([for e in google_cloud_run_v2_service.main.template[0].containers[0].env : e.value if e.name == "SCHEDULED_TASK_OIDC_AUDIENCE"]) == "cudly-test/api/scheduled/ri_exchange_reshape"
    error_message = "With the scheduler off only the RI exchange audience may be accepted."
  }
}
