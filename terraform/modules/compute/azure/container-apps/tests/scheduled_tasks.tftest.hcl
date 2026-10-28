# Scheduled server sweeps on Container Apps (#712). Every provider is mocked;
# nothing here reaches Azure. Each sweep must be a Logic App workflow that
# reads the scheduled-task secret with its own identity and POSTs to
# /api/scheduled/<task>, on a recurrence equal to its cron schedule, and the
# money sweeps must stay off unless asked for.
mock_provider "azurerm" {
  # Resources referenced by ID elsewhere in the module are parsed, so the mock
  # needs well-formed ARM IDs.
  mock_resource "azurerm_user_assigned_identity" {
    defaults = {
      id           = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/cudly-test-rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/cudly-test-identity"
      principal_id = "22222222-2222-2222-2222-222222222222"
      client_id    = "33333333-3333-3333-3333-333333333333"
    }
  }
  mock_resource "azurerm_logic_app_workflow" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/cudly-test-rg/providers/Microsoft.Logic/workflows/cudly-test-workflow"
    }
  }
  mock_resource "azurerm_container_app_environment" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/cudly-test-rg/providers/Microsoft.App/managedEnvironments/cudly-test-env"
    }
  }
}

variables {
  app_name                       = "cudly-test"
  environment                    = "dev"
  resource_group_name            = "cudly-test-rg"
  location                       = "eastus2"
  image_uri                      = "cudlytest.azurecr.io/cudly:test"
  database_host                  = "cudly-test-postgres.postgres.database.azure.com"
  database_name                  = "cudly"
  database_username              = "cudlyadmin"
  database_password_secret_name  = "database-password"
  subscription_id                = "00000000-0000-0000-0000-000000000000"
  reservation_role_definition_id = "/subscriptions/00000000-0000-0000-0000-000000000000/providers/Microsoft.Authorization/roleDefinitions/11111111-1111-1111-1111-111111111111"
  key_vault_uri                  = "https://cudly-test-kv.vault.azure.net/"
  key_vault_id                   = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/cudly-test-rg/providers/Microsoft.KeyVault/vaults/cudly-test-kv"
  signing_key_name               = "oidc-signing-key"
  registry_server                = "cudlytest.azurecr.io"
  container_registry_id          = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/cudly-test-rg/providers/Microsoft.ContainerRegistry/registries/cudlytest"
  admin_email                    = "admin@example.com"
  admin_password_secret_name     = "admin-password"
  enable_scheduled_tasks         = true
  scheduled_task_secret_name     = "scheduled-task-secret"
  infrastructure_subnet_id       = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/cudly-test-rg/providers/Microsoft.Network/virtualNetworks/vnet/subnets/apps"
  internal_load_balancer_enabled = false
}

run "defaults_schedule_only_the_default_on_sweeps" {
  command = apply

  assert {
    condition     = toset(keys(azurerm_logic_app_workflow.sweep)) == toset(["analytics_collect", "finalize_revocations", "reap_stuck_purchases"])
    error_message = "By default only reap, analytics and finalize run; fire, process and send_notifications are opt-in."
  }

  assert {
    condition = alltrue([for k, a in azurerm_logic_app_action_custom.sweep :
      jsondecode(a.body).inputs.method == "POST" &&
      endswith(jsondecode(a.body).inputs.uri, "/api/scheduled/${k}") &&
      jsondecode(a.body).inputs.headers.Authorization == "Bearer @{body('get-secret')['value']}" &&
      a.logic_app_id == azurerm_logic_app_workflow.sweep[k].id
    ])
    error_message = "Each sweep workflow must POST to /api/scheduled/<its task> with the secret fetched by get-secret."
  }

  assert {
    condition = alltrue([for k, a in azurerm_logic_app_action_custom.sweep_get_secret :
      jsondecode(a.body).inputs.uri == "https://cudly-test-kv.vault.azure.net/secrets/scheduled-task-secret?api-version=7.4" &&
      jsondecode(a.body).inputs.authentication.identity == azurerm_user_assigned_identity.sweep[k].id &&
      azurerm_role_assignment.sweep_kv_secrets_user[k].principal_id == azurerm_user_assigned_identity.sweep[k].principal_id &&
      azurerm_role_assignment.sweep_kv_secrets_user[k].role_definition_name == "Key Vault Secrets User"
    ])
    error_message = "Each sweep must read the secret by URL (never by value) with its own identity, which holds Key Vault Secrets User."
  }

  # Default cron schedules map to equal recurrences.
  assert {
    condition = (
      azurerm_logic_app_trigger_recurrence.sweep["reap_stuck_purchases"].frequency == "Minute" &&
      azurerm_logic_app_trigger_recurrence.sweep["reap_stuck_purchases"].interval == 5 &&
      azurerm_logic_app_trigger_recurrence.sweep["reap_stuck_purchases"].start_time == "2024-01-01T00:00:00Z"
    )
    error_message = "*/5 * * * * must become every 5 minutes."
  }

  assert {
    condition = (
      azurerm_logic_app_trigger_recurrence.sweep["finalize_revocations"].frequency == "Hour" &&
      azurerm_logic_app_trigger_recurrence.sweep["finalize_revocations"].interval == 1 &&
      azurerm_logic_app_trigger_recurrence.sweep["finalize_revocations"].start_time == "2024-01-01T00:30:00Z"
    )
    error_message = "30 * * * * must become hourly at minute 30."
  }

  assert {
    condition = (
      azurerm_logic_app_trigger_recurrence.sweep["analytics_collect"].frequency == "Day" &&
      azurerm_logic_app_trigger_recurrence.sweep["analytics_collect"].interval == 1 &&
      azurerm_logic_app_trigger_recurrence.sweep["analytics_collect"].start_time == "2024-01-01T04:00:00Z"
    )
    error_message = "0 4 * * * must become daily at 04:00."
  }

  # The pre-#712 workflows are unchanged.
  assert {
    condition     = length(azurerm_logic_app_workflow.recommendations) == 1 && length(azurerm_logic_app_workflow.cleanup) == 1
    error_message = "The recommendations and cleanup workflows must still exist."
  }
}

run "opt_in_sweeps_follow_their_flags" {
  command = apply

  variables {
    enable_fire_scheduled_purchases_schedule    = true
    enable_process_scheduled_purchases_schedule = true
    enable_send_notifications_schedule          = true
    enable_finalize_revocations_schedule        = false
    process_scheduled_purchases_schedule        = "15 */6 * * *"
  }

  assert {
    condition = toset(keys(azurerm_logic_app_workflow.sweep)) == toset([
      "analytics_collect", "fire_scheduled_purchases", "process_scheduled_purchases", "reap_stuck_purchases", "send_notifications",
    ])
    error_message = "Each per-task flag must switch its workflow on or off independently."
  }

  assert {
    condition = (
      azurerm_logic_app_trigger_recurrence.sweep["process_scheduled_purchases"].frequency == "Hour" &&
      azurerm_logic_app_trigger_recurrence.sweep["process_scheduled_purchases"].interval == 6 &&
      azurerm_logic_app_trigger_recurrence.sweep["process_scheduled_purchases"].start_time == "2024-01-01T00:15:00Z"
    )
    error_message = "15 */6 * * * must become every 6 hours from 00:15."
  }

  assert {
    condition     = azurerm_logic_app_trigger_recurrence.sweep["fire_scheduled_purchases"].frequency == "Minute" && azurerm_logic_app_trigger_recurrence.sweep["fire_scheduled_purchases"].interval == 15
    error_message = "*/15 * * * * must become every 15 minutes."
  }
}

run "scheduler_off_creates_no_sweeps" {
  command = plan

  variables {
    enable_scheduled_tasks                   = false
    enable_fire_scheduled_purchases_schedule = true
  }

  assert {
    condition     = length(azurerm_logic_app_workflow.sweep) == 0 && length(azurerm_logic_app_action_custom.sweep) == 0
    error_message = "enable_scheduled_tasks is the master switch: with it off no sweep workflow may exist, even with a per-task flag on."
  }
}

run "secret_name_required_with_the_scheduler_on" {
  command = plan

  variables {
    scheduled_task_secret_name = ""
  }

  expect_failures = [terraform_data.scheduled_task_secret_preconditions]
}

# Shapes a Logic Apps recurrence cannot reproduce exactly fail the plan
# instead of running on a different cadence.
run "rejects_day_of_week_schedule" {
  command = plan

  variables {
    reap_stuck_purchases_schedule = "*/5 * * * 1"
  }

  expect_failures = [azurerm_logic_app_trigger_recurrence.sweep["reap_stuck_purchases"]]
}

run "rejects_minute_step_within_an_hour_range" {
  command = plan

  variables {
    reap_stuck_purchases_schedule = "*/5 2 * * *"
  }

  expect_failures = [azurerm_logic_app_trigger_recurrence.sweep["reap_stuck_purchases"]]
}

run "rejects_minute_step_not_dividing_an_hour" {
  command = plan

  variables {
    reap_stuck_purchases_schedule = "*/7 * * * *"
  }

  expect_failures = [azurerm_logic_app_trigger_recurrence.sweep["reap_stuck_purchases"]]
}

run "rejects_out_of_range_hour" {
  command = plan

  variables {
    analytics_collect_schedule = "0 24 * * *"
  }

  expect_failures = [azurerm_logic_app_trigger_recurrence.sweep["analytics_collect"]]
}

run "rejects_eventbridge_rate" {
  command = plan

  variables {
    finalize_revocations_schedule = "rate(1 hour)"
  }

  expect_failures = [azurerm_logic_app_trigger_recurrence.sweep["finalize_revocations"]]
}
