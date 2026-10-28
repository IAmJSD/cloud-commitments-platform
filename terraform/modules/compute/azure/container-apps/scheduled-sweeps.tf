# Scheduled server sweeps on Container Apps (#712).
#
# Each enabled sweep gets the same Logic App shape as the recommendations
# workflow in scheduled-tasks.tf: its own user-assigned identity holding Key
# Vault Secrets User, a recurrence trigger, a `get-secret` action that reads
# the scheduled-task secret at run time, and an HTTP action that POSTs to
# /api/scheduled/<task> with it as the bearer token. The secret never lands in
# the workflow definition or Terraform state (#183).
#
# enable_scheduled_tasks is the master switch, as for the recommendations and
# cleanup workflows: with it off, no sweep workflow is created whatever the
# per-task flags say. That also keeps the secret-name precondition in
# scheduled-tasks.tf covering every workflow that reads the secret.

locals {
  # Keyed by server task name. `job` is the workflow name suffix. Default-on
  # tasks match the other platforms; fire_scheduled_purchases,
  # process_scheduled_purchases and send_notifications are opt-in (default
  # off, as decided for #712). cleanup has its own workflow in
  # scheduled-tasks.tf.
  sweep_tasks = {
    reap_stuck_purchases        = { job = "reap", enabled = var.enable_reap_stuck_purchases_schedule, schedule = var.reap_stuck_purchases_schedule }
    analytics_collect           = { job = "analytics", enabled = var.enable_analytics_collect_schedule, schedule = var.analytics_collect_schedule }
    fire_scheduled_purchases    = { job = "fire", enabled = var.enable_fire_scheduled_purchases_schedule, schedule = var.fire_scheduled_purchases_schedule }
    process_scheduled_purchases = { job = "process", enabled = var.enable_process_scheduled_purchases_schedule, schedule = var.process_scheduled_purchases_schedule }
    send_notifications          = { job = "notify", enabled = var.enable_send_notifications_schedule, schedule = var.send_notifications_schedule }
    finalize_revocations        = { job = "finalize", enabled = var.enable_finalize_revocations_schedule, schedule = var.finalize_revocations_schedule }
  }
  active_sweeps = var.enable_scheduled_tasks ? { for k, v in local.sweep_tasks : k => v if v.enabled } : {}

  # Cron to Logic Apps recurrence. Captures: [0] minute field, [1] hour field.
  # An unsupported expression yields no match and a null recurrence, which
  # the trigger's precondition rejects before the trigger is evaluated.
  sweep_cron = {
    for k, v in local.active_sweeps : k => regexall("^(\\*/[0-9]+|[0-9]+)\\s+(\\*|\\*/[0-9]+|[0-9]+)\\s+\\*\\s+\\*\\s+\\*$", trimspace(v.schedule))
  }
  sweep_recurrence = {
    for k, m in local.sweep_cron : k => (
      length(m) == 0 ? null :
      startswith(m[0][0], "*/") ? (
        # "*/N * * * *": every N minutes from the top of the hour.
        m[0][1] == "*" ? { frequency = "Minute", interval = tonumber(trimprefix(m[0][0], "*/")), hour = 0, minute = 0 } : null
      ) :
      # "M * * * *": hourly at minute M.
      m[0][1] == "*" ? { frequency = "Hour", interval = 1, hour = 0, minute = tonumber(m[0][0]) } :
      # "M */N * * *": every N hours from midnight, at minute M.
      startswith(m[0][1], "*/") ? { frequency = "Hour", interval = tonumber(trimprefix(m[0][1], "*/")), hour = 0, minute = tonumber(m[0][0]) } :
      # "M H * * *": daily at H:M.
      { frequency = "Day", interval = 1, hour = tonumber(m[0][1]), minute = tonumber(m[0][0]) }
    )
  }
}

resource "azurerm_user_assigned_identity" "sweep" {
  for_each = local.active_sweeps

  name                = "${var.app_name}-${each.value.job}-uami"
  location            = var.location
  resource_group_name = var.resource_group_name
  tags                = var.tags
}

resource "azurerm_role_assignment" "sweep_kv_secrets_user" {
  for_each = local.active_sweeps

  scope                = var.key_vault_id
  role_definition_name = "Key Vault Secrets User"
  principal_id         = azurerm_user_assigned_identity.sweep[each.key].principal_id
}

resource "azurerm_logic_app_workflow" "sweep" {
  for_each = local.active_sweeps

  name                = "${var.app_name}-${each.value.job}"
  location            = var.location
  resource_group_name = var.resource_group_name

  identity {
    type         = "UserAssigned"
    identity_ids = [azurerm_user_assigned_identity.sweep[each.key].id]
  }

  tags = var.tags
}

resource "azurerm_logic_app_trigger_recurrence" "sweep" {
  for_each = local.active_sweeps

  name         = "${each.value.job}-trigger"
  logic_app_id = azurerm_logic_app_workflow.sweep[each.key].id
  frequency    = local.sweep_recurrence[each.key].frequency
  interval     = local.sweep_recurrence[each.key].interval
  # A fixed past start time anchors the recurrence (the minute and hour
  # offsets come from it) without the timestamp() drift of the older
  # triggers, which rewrite start_time on every apply.
  start_time = format("2024-01-01T%02d:%02d:00Z", local.sweep_recurrence[each.key].hour, local.sweep_recurrence[each.key].minute)
  time_zone  = "UTC"

  lifecycle {
    precondition {
      condition = local.sweep_recurrence[each.key] != null && try(
        local.sweep_recurrence[each.key].minute <= 59 &&
        local.sweep_recurrence[each.key].hour <= 23 &&
        local.sweep_recurrence[each.key].interval >= 1 &&
        (local.sweep_recurrence[each.key].frequency != "Minute" || 60 % local.sweep_recurrence[each.key].interval == 0) &&
        (local.sweep_recurrence[each.key].frequency != "Hour" || 24 % local.sweep_recurrence[each.key].interval == 0),
        false
      )
      error_message = "Container Apps sweep schedules must be 5-field cron of the form \"*/N * * * *\" (N divides 60), \"M * * * *\", \"M */N * * *\" (N divides 24) or \"M H * * *\", with M in 0-59 and H in 0-23."
    }
  }
}

resource "azurerm_logic_app_action_custom" "sweep_get_secret" {
  for_each = local.active_sweeps

  name         = "get-secret"
  logic_app_id = azurerm_logic_app_workflow.sweep[each.key].id

  body = jsonencode({
    type = "Http"
    inputs = {
      method = "GET"
      uri    = local.scheduled_task_secret_url
      authentication = {
        type     = "ManagedServiceIdentity"
        audience = "https://vault.azure.net"
        identity = azurerm_user_assigned_identity.sweep[each.key].id
      }
    }
    runAfter = {}
    runtimeConfiguration = {
      secureData = {
        properties = ["outputs"]
      }
    }
  })

  # See recommendations_get_secret.depends_on rationale.
  depends_on = [azurerm_role_assignment.sweep_kv_secrets_user]
}

resource "azurerm_logic_app_action_custom" "sweep" {
  for_each = local.active_sweeps

  name         = "call-endpoint"
  logic_app_id = azurerm_logic_app_workflow.sweep[each.key].id

  body = jsonencode({
    type = "Http"
    inputs = {
      method = "POST"
      uri    = "https://${azurerm_container_app.main.ingress[0].fqdn}/api/scheduled/${each.key}"
      headers = {
        "Content-Type"  = "application/json"
        "Authorization" = "Bearer @{body('get-secret')['value']}"
        "X-Trigger"     = "scheduled"
        "X-Source"      = "azure-logic-apps"
      }
      body = {
        source    = "azure-logic-apps"
        timestamp = "@{utcNow()}"
      }
    }
    runAfter = {
      "get-secret" = ["Succeeded"]
    }
    runtimeConfiguration = {
      secureData = {
        properties = ["inputs"]
      }
    }
  })

  depends_on = [azurerm_logic_app_action_custom.sweep_get_secret]
}
