# ==============================================
# Failure alerts (issue #714)
# ==============================================
#
# The application logs a stable marker token for the two failures operators
# must hear about; each becomes a log-query alert that fires on any
# occurrence:
#
#   purchase_execution_failed  an execution row ended failed or
#                              partially_completed
#                              (internal/purchase/failure_notify.go)
#   scheduled_task_failed      a scheduled task returned an error, including a
#                              process_scheduled_purchases run with failures
#                              (internal/server/scheduled_tasks.go)
#
# Container Apps writes the app's stdout/stderr to ContainerAppConsoleLogs_CL
# in the environment's Log Analytics workspace (var.app_log_analytics_workspace_id),
# not to Application Insights, so these alerts are scoped to that workspace.
# TestMonitoringLogMarkersMatchGo
# (terraform/modules/monitoring/log_marker_contract_test.go) keeps the tokens
# in step with the Go code.

locals {
  failure_markers = {
    purchase-failed = {
      token       = "purchase_execution_failed"
      description = "A purchase execution ended failed or partially_completed. Search ContainerAppConsoleLogs_CL for purchase_execution_failed to find the execution_id."
    }
    scheduled-task-failed = {
      token       = "scheduled_task_failed"
      description = "A scheduled task returned an error. Search ContainerAppConsoleLogs_CL for scheduled_task_failed for the task and error."
    }
  }
}

resource "azurerm_monitor_scheduled_query_rules_alert_v2" "failure_marker" {
  for_each = local.failure_markers

  name                = "${var.app_name}-${each.key}"
  resource_group_name = var.resource_group_name
  location            = var.location

  evaluation_frequency = "PT5M"
  window_duration      = "PT5M"
  scopes               = [var.app_log_analytics_workspace_id]
  severity             = 1
  description          = each.value.description

  criteria {
    query                   = <<-QUERY
      ContainerAppConsoleLogs_CL
      | where Log_s contains "${each.value.token}"
    QUERY
    time_aggregation_method = "Count"
    threshold               = 0
    operator                = "GreaterThan"

    failing_periods {
      minimum_failing_periods_to_trigger_alert = 1
      number_of_evaluation_periods             = 1
    }
  }

  action {
    action_groups = concat(
      [azurerm_monitor_action_group.email.id],
      var.slack_webhook_url != "" ? [azurerm_monitor_action_group.slack[0].id] : []
    )
  }

  tags = var.tags
}
