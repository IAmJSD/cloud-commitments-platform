# Issue #714: a failed purchase or scheduled task must raise an alert that
# reaches the configured email receivers. The app's console logs live in the
# Container Apps environment's workspace, so that is what the alerts query.
mock_provider "azurerm" {
  # The provider validates these as resource IDs; the mock's random strings fail.
  mock_resource "azurerm_monitor_action_group" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-cudly-test/providers/Microsoft.Insights/actionGroups/cudly-test-email-alerts"
    }
  }
  mock_resource "azurerm_application_insights" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-cudly-test/providers/Microsoft.Insights/components/cudly-test-insights"
    }
  }
  mock_resource "azurerm_application_insights_standard_web_test" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-cudly-test/providers/Microsoft.Insights/webTests/cudly-test-health"
    }
  }
  mock_resource "azurerm_log_analytics_workspace" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-cudly-test/providers/Microsoft.OperationalInsights/workspaces/cudly-test-logs"
    }
  }
}

variables {
  app_name                       = "cudly-test"
  environment                    = "dev"
  location                       = "eastus"
  resource_group_name            = "rg-cudly-test"
  container_app_id               = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-cudly-test/providers/Microsoft.App/containerApps/cudly-test"
  db_server_id                   = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-cudly-test/providers/Microsoft.DBforPostgreSQL/flexibleServers/cudly-test-db"
  app_url                        = "https://cudly-test.example.com"
  app_log_analytics_workspace_id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-cudly-test/providers/Microsoft.OperationalInsights/workspaces/cudly-test-env-logs"
  alert_email_addresses          = ["alerts-a@example.com", "alerts-b@example.com"]
}

run "failure_markers_raise_alerts" {
  command = apply

  assert {
    condition     = strcontains(azurerm_monitor_scheduled_query_rules_alert_v2.failure_marker["purchase-failed"].criteria[0].query, "Log_s contains \"purchase_execution_failed\"")
    error_message = "The purchase-failure alert must match the purchase_execution_failed marker in the console logs."
  }

  assert {
    condition     = strcontains(azurerm_monitor_scheduled_query_rules_alert_v2.failure_marker["scheduled-task-failed"].criteria[0].query, "Log_s contains \"scheduled_task_failed\"")
    error_message = "The scheduled-task alert must match the scheduled_task_failed marker."
  }

  assert {
    condition = alltrue([
      for a in azurerm_monitor_scheduled_query_rules_alert_v2.failure_marker :
      length(a.scopes) == 1 && contains(a.scopes, var.app_log_analytics_workspace_id) &&
      strcontains(a.criteria[0].query, "ContainerAppConsoleLogs_CL") &&
      a.criteria[0].time_aggregation_method == "Count" &&
      a.criteria[0].operator == "GreaterThan" && a.criteria[0].threshold == 0
    ])
    error_message = "Each alert must count marker lines in the app's console-log workspace and fire on any occurrence."
  }

  assert {
    condition = alltrue([
      for a in azurerm_monitor_scheduled_query_rules_alert_v2.failure_marker :
      length(a.action[0].action_groups) == 1 && contains(a.action[0].action_groups, azurerm_monitor_action_group.email.id)
    ])
    error_message = "The failure alerts must notify the email action group."
  }

  assert {
    condition = (
      length(azurerm_monitor_action_group.email.email_receiver) == 2 &&
      toset([for r in azurerm_monitor_action_group.email.email_receiver : r.email_address]) == toset(var.alert_email_addresses)
    )
    error_message = "Each configured address needs an email receiver on the action group."
  }
}
