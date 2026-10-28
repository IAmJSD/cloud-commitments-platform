# Scheduled server tasks on AKS (#893).
#
# Each enabled task is a Kubernetes CronJob that starts a one-off pod from the
# API image and runs `/app/cudly --task <name>`, the same entry point the
# Fargate EventBridge targets use. The task runs in-process against the
# database, so no HTTP call, ingress route or scheduled-task secret is
# involved. The pod uses the API's service account (the only one the workload
# identity's federated credential accepts) and the API's environment
# (local.app_env in main.tf), so it reaches the database and Key Vault the
# same way the API does.
#
# Like the Deployment, the CronJobs exist only when
# var.deploy_kubernetes_resources is true. Schedules are standard 5-field cron
# expressions, evaluated by the cluster in UTC.

locals {
  # Keyed by server task name. `job` is the CronJob name suffix. Default-on
  # tasks match the other platforms, including fire_scheduled_purchases;
  # process_scheduled_purchases, send_notifications and cleanup are opt-in
  # (default off, as decided for #712). ri_exchange_reshape (as on Container
  # Apps, Cloud Run and Fargate) and ladder_run (as on Fargate and Lambda) are
  # opt-in features, off by default.
  sweep_tasks = {
    collect_recommendations     = { job = "collect", enabled = var.enable_scheduled_tasks, schedule = var.recommendation_schedule }
    reap_stuck_purchases        = { job = "reap", enabled = var.enable_reap_stuck_purchases_schedule, schedule = var.reap_stuck_purchases_schedule }
    analytics_collect           = { job = "analytics", enabled = var.enable_analytics_collect_schedule, schedule = var.analytics_collect_schedule }
    fire_scheduled_purchases    = { job = "fire", enabled = var.enable_fire_scheduled_purchases_schedule, schedule = var.fire_scheduled_purchases_schedule }
    process_scheduled_purchases = { job = "process", enabled = var.enable_process_scheduled_purchases_schedule, schedule = var.process_scheduled_purchases_schedule }
    send_notifications          = { job = "notify", enabled = var.enable_send_notifications_schedule, schedule = var.send_notifications_schedule }
    finalize_revocations        = { job = "finalize", enabled = var.enable_finalize_revocations_schedule, schedule = var.finalize_revocations_schedule }
    cleanup                     = { job = "cleanup", enabled = var.enable_cleanup_schedule, schedule = var.cleanup_schedule }
    ri_exchange_reshape         = { job = "ri-exchange", enabled = var.enable_ri_exchange_schedule, schedule = var.ri_exchange_schedule }
    ladder_run                  = { job = "ladder", enabled = var.enable_ladder_run_schedule, schedule = var.ladder_run_schedule }
  }
  active_sweeps = var.deploy_kubernetes_resources ? { for k, v in local.sweep_tasks : k => v if v.enabled } : {}

  # The server stops a task at TASK_TIMEOUT; the Job deadline adds headroom
  # for startup (database connection, migrations, secret resolution) so a pod
  # that hangs before or after the task is still killed.
  task_job_deadline_seconds = var.task_timeout + 300

  # TASK_TIMEOUT is owned by var.task_timeout, so an additional_env_vars entry
  # of that name is dropped rather than duplicated.
  task_env = concat(
    [for e in local.app_env : e if e.name != "TASK_TIMEOUT"],
    [{ name = "TASK_TIMEOUT", value = tostring(var.task_timeout), secret_ref = null }],
  )
}

resource "kubernetes_cron_job_v1" "sweep" {
  for_each = local.active_sweeps

  metadata {
    name      = "${var.project_name}-task-${each.value.job}"
    namespace = kubernetes_namespace.app[0].metadata[0].name

    labels = {
      app         = "${var.project_name}-task"
      task        = each.value.job
      environment = var.environment
    }
  }

  spec {
    schedule = each.value.schedule
    # A run still going when the next one is due makes the next one skip,
    # rather than stacking pods. The server's per-task advisory lock already
    # makes an overlapping run a no-op; this keeps the pods from piling up.
    concurrency_policy            = "Forbid"
    starting_deadline_seconds     = 300
    successful_jobs_history_limit = 1
    failed_jobs_history_limit     = 3

    job_template {
      metadata {
        labels = {
          app  = "${var.project_name}-task"
          task = each.value.job
        }
      }

      spec {
        # No in-Job retry: a failed run is retried by the next schedule, the
        # same as the EventBridge-triggered Fargate tasks.
        backoff_limit           = 0
        active_deadline_seconds = local.task_job_deadline_seconds

        template {
          metadata {
            # Not the API's `app` label: the Service and the NGINX network
            # policy select on that, and must never pick up a task pod.
            labels = {
              app         = "${var.project_name}-task"
              task        = each.value.job
              environment = var.environment
              # Opts the pod into the workload identity webhook (#198), as on
              # the Deployment.
              "azure.workload.identity/use" = "true"
            }
          }

          spec {
            service_account_name = kubernetes_service_account.app[0].metadata[0].name
            restart_policy       = "Never"

            container {
              name  = "task"
              image = "${var.image_name}:${var.image_tag}"
              # Bypasses /entrypoint.sh, which would append --mode/--port and
              # start the HTTP server path; --task runs one task and exits.
              command = ["/app/cudly", "--task", each.key]

              dynamic "env" {
                for_each = local.task_env
                content {
                  name  = env.value.name
                  value = env.value.secret_ref == null ? env.value.value : null

                  dynamic "value_from" {
                    for_each = env.value.secret_ref == null ? [] : [env.value.secret_ref]
                    content {
                      secret_key_ref {
                        name = kubernetes_secret.database[0].metadata[0].name
                        key  = value_from.value
                      }
                    }
                  }
                }
              }

              # Requests and limits are required: the namespace ResourceQuota
              # rejects pods without them. Same sizing as an API replica.
              resources {
                requests = {
                  cpu    = "250m"
                  memory = "512Mi"
                }
                limits = {
                  cpu    = "1000m"
                  memory = "1Gi"
                }
              }
            }
          }
        }
      }
    }
  }

  lifecycle {
    precondition {
      # Kubernetes appends an 11-character suffix to a CronJob's name for each
      # Job, so the CronJob name itself is limited to 52 characters.
      condition     = length("${var.project_name}-task-${each.value.job}") <= 52
      error_message = "project_name is too long for the scheduled-task CronJob names (at most 52 characters including \"-task-<job>\")."
    }
    precondition {
      condition     = can(regex("^(@(yearly|annually|monthly|weekly|daily|midnight|hourly)|\\S+(\\s+\\S+){4})$", trimspace(each.value.schedule)))
      error_message = "Scheduled-task schedules on AKS must be 5-field cron expressions (for example \"*/5 * * * *\"), not EventBridge rate() or 6-field cron()."
    }
  }

  # Same ordering reasons as the Deployment: the pod reads Key Vault through
  # the workload identity, so the grant and the federated credential should
  # exist before the first run.
  depends_on = [
    kubernetes_secret.database,
    azurerm_role_assignment.workload_kv_secrets_user,
    azurerm_federated_identity_credential.workload,
  ]
}
