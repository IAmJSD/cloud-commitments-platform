# Module wiring for the scheduled-task CronJobs (#893). Plans are targeted:
# the mock azurerm provider plans kubelet_identity as an empty list, which
# azurerm_role_assignment.aks_acr indexes, and that resource is not under test.
# The Go test in terraform/modules/compute checks the static wiring.
mock_provider "azurerm" {}
mock_provider "kubernetes" {}
mock_provider "helm" {}
mock_provider "time" {}

variables {
  project_name                  = "cudly"
  environment                   = "dev"
  resource_group_name           = "rg"
  location                      = "eastus"
  vnet_subnet_id                = "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet/subnets/aks"
  image_name                    = "cudlyacr.azurecr.io/cudly"
  image_tag                     = "abc123"
  database_host                 = "db.example.com"
  database_name                 = "cudly"
  database_username             = "cudly"
  database_password_secret_name = "db-password"
  key_vault_id                  = "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.KeyVault/vaults/kv"
  key_vault_uri                 = "https://kv.vault.azure.net/"
  admin_email                   = "admin@example.com"
  admin_password_secret_name    = "admin-password"
  allowed_origins               = ["https://app.example.com"]
  archera_org_id                = "org"
  additional_env_vars           = { SCHEDULED_TASK_SECRET_NAME = "scheduled-task-secret", CUDLY_SOURCE_CLOUD = "azure" }
  deploy_kubernetes_resources   = true
}

run "cluster_only" {
  command = plan

  variables {
    deploy_kubernetes_resources = false
  }

  plan_options {
    target = [kubernetes_cron_job_v1.sweep]
  }

  assert {
    condition     = length(kubernetes_cron_job_v1.sweep) == 0
    error_message = "No CronJob without deploy_kubernetes_resources (there is no namespace or service account to run in)."
  }
}

run "defaults" {
  command = plan

  plan_options {
    target = [kubernetes_deployment.app, kubernetes_service.app, kubernetes_cron_job_v1.sweep]
  }

  assert {
    condition     = toset(keys(kubernetes_cron_job_v1.sweep)) == toset(["analytics_collect", "collect_recommendations", "finalize_revocations", "reap_stuck_purchases"])
    error_message = "Default-on tasks are collect_recommendations, reap_stuck_purchases, analytics_collect and finalize_revocations; fire, process, send_notifications and cleanup are opt-in."
  }
  assert {
    condition = alltrue([
      for k, j in kubernetes_cron_job_v1.sweep :
      j.spec[0].job_template[0].spec[0].template[0].spec[0].container[0].command == tolist(["/app/cudly", "--task", k])
    ])
    error_message = "Each CronJob must run /app/cudly --task <its task name>."
  }
  assert {
    condition = alltrue([
      for k, j in kubernetes_cron_job_v1.sweep :
      j.spec[0].schedule == { collect_recommendations = var.recommendation_schedule, reap_stuck_purchases = var.reap_stuck_purchases_schedule, analytics_collect = var.analytics_collect_schedule, finalize_revocations = var.finalize_revocations_schedule }[k]
    ])
    error_message = "Each schedule must come from its variable."
  }
  assert {
    condition     = toset([for j in kubernetes_cron_job_v1.sweep : j.metadata[0].name]) == toset(["cudly-task-collect", "cudly-task-reap", "cudly-task-analytics", "cudly-task-finalize"])
    error_message = "CronJob names must be distinct <project>-task-<job> names."
  }
  assert {
    condition = alltrue([
      for j in kubernetes_cron_job_v1.sweep :
      j.metadata[0].namespace == "dev" && j.spec[0].concurrency_policy == "Forbid" && j.spec[0].job_template[0].spec[0].backoff_limit == 0 && j.spec[0].job_template[0].spec[0].active_deadline_seconds == 1200 && j.spec[0].job_template[0].spec[0].template[0].spec[0].restart_policy == "Never"
    ])
    error_message = "CronJobs run in the app namespace, never overlap, do not retry in-Job, are killed after task_timeout + 300s and do not restart."
  }
  assert {
    condition = alltrue([
      for j in kubernetes_cron_job_v1.sweep :
      j.spec[0].job_template[0].spec[0].template[0].spec[0].service_account_name == "cudly-api" && j.spec[0].job_template[0].spec[0].template[0].metadata[0].labels["azure.workload.identity/use"] == "true"
    ])
    error_message = "Task pods must use the API service account (the federated credential's subject) and opt into workload identity."
  }
  assert {
    condition = alltrue([
      for j in kubernetes_cron_job_v1.sweep :
      j.spec[0].job_template[0].spec[0].template[0].metadata[0].labels["app"] != kubernetes_service.app[0].spec[0].selector["app"]
    ])
    error_message = "Task pods must not match the API Service selector, or the Service would route traffic to them."
  }
  assert {
    condition = alltrue([
      for j in kubernetes_cron_job_v1.sweep :
      j.spec[0].job_template[0].spec[0].template[0].spec[0].container[0].image == "cudlyacr.azurecr.io/cudly:abc123"
    ])
    error_message = "Task pods must run the API image."
  }
  assert {
    condition = alltrue([
      for j in kubernetes_cron_job_v1.sweep : alltrue([
        for e in kubernetes_deployment.app[0].spec[0].template[0].spec[0].container[0].env :
        contains(j.spec[0].job_template[0].spec[0].template[0].spec[0].container[0].env, e)
      ])
    ])
    error_message = "Task pods must carry every environment variable of the API container, secret references included."
  }
  assert {
    condition = alltrue([
      for j in kubernetes_cron_job_v1.sweep :
      length(j.spec[0].job_template[0].spec[0].template[0].spec[0].container[0].env) == length(kubernetes_deployment.app[0].spec[0].template[0].spec[0].container[0].env) + 1 &&
      [for e in j.spec[0].job_template[0].spec[0].template[0].spec[0].container[0].env : e.value if e.name == "TASK_TIMEOUT"] == ["900"]
    ])
    error_message = "Task pods add only TASK_TIMEOUT, set from task_timeout."
  }
  assert {
    condition = alltrue([
      for j in kubernetes_cron_job_v1.sweep :
      j.spec[0].job_template[0].spec[0].template[0].spec[0].container[0].resources[0].requests["cpu"] == "250m" && j.spec[0].job_template[0].spec[0].template[0].spec[0].container[0].resources[0].limits["memory"] == "1Gi"
    ])
    error_message = "Task pods need requests and limits, or the namespace ResourceQuota rejects them."
  }
}

run "opt_in" {
  command = plan

  variables {
    enable_fire_scheduled_purchases_schedule    = true
    enable_process_scheduled_purchases_schedule = true
    enable_send_notifications_schedule          = true
    enable_cleanup_schedule                     = true
  }

  plan_options {
    target = [kubernetes_cron_job_v1.sweep]
  }

  assert {
    condition     = toset(keys(kubernetes_cron_job_v1.sweep)) == toset(["analytics_collect", "cleanup", "collect_recommendations", "finalize_revocations", "fire_scheduled_purchases", "process_scheduled_purchases", "reap_stuck_purchases", "send_notifications"])
    error_message = "Opting in must schedule all eight required tasks."
  }
  assert {
    condition = alltrue([
      for k, j in kubernetes_cron_job_v1.sweep :
      j.spec[0].job_template[0].spec[0].template[0].spec[0].container[0].command == tolist(["/app/cudly", "--task", k])
    ])
    error_message = "Each CronJob must run /app/cudly --task <its task name>."
  }
  assert {
    condition = alltrue([
      for k, j in kubernetes_cron_job_v1.sweep :
      j.spec[0].schedule == { collect_recommendations = var.recommendation_schedule, reap_stuck_purchases = var.reap_stuck_purchases_schedule, analytics_collect = var.analytics_collect_schedule, fire_scheduled_purchases = var.fire_scheduled_purchases_schedule, process_scheduled_purchases = var.process_scheduled_purchases_schedule, send_notifications = var.send_notifications_schedule, finalize_revocations = var.finalize_revocations_schedule, cleanup = var.cleanup_schedule }[k]
    ])
    error_message = "Each schedule must come from its variable."
  }
  assert {
    condition     = length(distinct([for j in kubernetes_cron_job_v1.sweep : j.metadata[0].name])) == 8
    error_message = "CronJob names must be distinct."
  }
}

run "all_off" {
  command = plan

  variables {
    enable_scheduled_tasks               = false
    enable_reap_stuck_purchases_schedule = false
    enable_analytics_collect_schedule    = false
    enable_finalize_revocations_schedule = false
  }

  plan_options {
    target = [kubernetes_cron_job_v1.sweep]
  }

  assert {
    condition     = length(kubernetes_cron_job_v1.sweep) == 0
    error_message = "Each enable variable must switch its CronJob off."
  }
}

run "task_timeout_owned_by_variable" {
  command = plan

  variables {
    task_timeout        = 600
    additional_env_vars = { TASK_TIMEOUT = "5" }
  }

  plan_options {
    target = [kubernetes_cron_job_v1.sweep]
  }

  assert {
    condition = alltrue([
      for j in kubernetes_cron_job_v1.sweep :
      [for e in j.spec[0].job_template[0].spec[0].template[0].spec[0].container[0].env : e.value if e.name == "TASK_TIMEOUT"] == ["600"] && j.spec[0].job_template[0].spec[0].active_deadline_seconds == 900
    ])
    error_message = "TASK_TIMEOUT must appear once, from task_timeout, with the Job deadline 300s past it."
  }
}

run "rejects_eventbridge_schedule" {
  command = plan

  variables {
    recommendation_schedule = "rate(1 day)"
  }

  plan_options {
    target = [kubernetes_cron_job_v1.sweep]
  }

  expect_failures = [kubernetes_cron_job_v1.sweep]
}

run "rejects_six_field_cron" {
  command = plan

  variables {
    reap_stuck_purchases_schedule = "0 */5 * * ? *"
  }

  plan_options {
    target = [kubernetes_cron_job_v1.sweep]
  }

  expect_failures = [kubernetes_cron_job_v1.sweep]
}

run "rejects_long_project_name" {
  command = plan

  variables {
    # 40 characters: within the cluster's 54-character dns_prefix, but
    # "-task-analytics" takes the CronJob name past 52.
    project_name = "cudly-a-long-project-name-for-the-limits"
  }

  plan_options {
    target = [kubernetes_cron_job_v1.sweep]
  }

  expect_failures = [kubernetes_cron_job_v1.sweep]
}

run "rejects_short_task_timeout" {
  command = plan

  variables {
    task_timeout = 30
  }

  plan_options {
    target = [kubernetes_cron_job_v1.sweep]
  }

  expect_failures = [var.task_timeout]
}
