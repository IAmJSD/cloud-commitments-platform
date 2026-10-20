# ==============================================
# Monitoring and failure alerts (issue #714)
# ==============================================
#
# Instantiates modules/monitoring/azure for the Container Apps platform: an
# email action group with one receiver per address in var.alert_emails,
# metric alerts on the container app and the PostgreSQL server, an
# availability test on /health, and log-query alerts on any failed purchase
# execution or failed scheduled task. The failure alerts query the Container
# Apps environment's Log Analytics workspace (module.networking), where the
# app's console logs land. The deploy service principal's custom role already
# covers Microsoft.Insights/* and Microsoft.OperationalInsights/workspaces/*
# (ci-cd-permissions/locals_networking.tf).
#
# Off by default so the owner decides when alerts start and who receives
# them: set the ALERT_EMAILS repository secret (comma-separated addresses)
# and enable_monitoring = true in the environment's tfvars. AKS is not wired:
# the module's metrics are Container Apps specific.

locals {
  alert_email_addresses = compact([for e in split(",", var.alert_emails) : trimspace(e)])
}

module "monitoring" {
  source = "../../modules/monitoring/azure"
  count  = var.enable_monitoring && var.compute_platform == "container-apps" ? 1 : 0

  app_name            = local.app_name
  environment         = var.environment
  location            = var.location
  resource_group_name = azurerm_resource_group.main.name
  container_app_id    = one(module.compute_container_apps[*].container_app_id)
  app_url             = one(module.compute_container_apps[*].container_app_url)
  db_server_id        = module.database.server_id

  app_log_analytics_workspace_id = module.networking.log_analytics_workspace_id

  alert_email_addresses = local.alert_email_addresses

  tags = local.common_tags
}

# Warns (never blocks) when prod would run with no alert recipient.
check "prod_alerts_reach_someone" {
  assert {
    condition     = !(var.environment == "prod" && (length(module.monitoring) == 0 || length(local.alert_email_addresses) == 0))
    error_message = "environment = \"prod\" has no alert recipient: set enable_monitoring = true (Container Apps) and the ALERT_EMAILS secret (issue #714)."
  }
}
