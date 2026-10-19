# ==============================================
# Monitoring and failure alerts (issue #714)
# ==============================================
#
# Instantiates modules/monitoring/gcp for the Cloud Run platform: one email
# notification channel per address in var.alert_emails, alert policies on the
# service and the Cloud SQL instance, an uptime check on /health, and
# log-based alerts on any failed purchase execution or failed scheduled task.
# The deploy service account already holds roles/monitoring.admin,
# roles/logging.admin and roles/pubsub.admin (ci-cd-permissions), so no
# bootstrap change is needed.
#
# Off by default so the owner decides when alerts start and who receives
# them: set the ALERT_EMAILS repository secret (comma-separated addresses)
# and enable_monitoring = true in the environment's tfvars. GKE is not wired:
# the module's metrics and filters are Cloud Run specific.

variable "enable_monitoring" {
  description = "Create the monitoring module (notification channels, alert policies, uptime check, log-based failure alerts). Cloud Run only."
  type        = bool
  default     = false
}

variable "alert_emails" {
  description = "Comma-separated email addresses that receive alerts. Supplied by the deploy workflow from the ALERT_EMAILS secret; never commit real addresses."
  type        = string
  default     = ""

  validation {
    condition = alltrue([
      for e in split(",", var.alert_emails) :
      trimspace(e) == "" || can(regex("^[^@\\s]+@[^@\\s]+\\.[^@\\s]+$", trimspace(e)))
    ])
    error_message = "alert_emails must be a comma-separated list of email addresses."
  }
}

locals {
  alert_email_addresses = compact([for e in split(",", var.alert_emails) : trimspace(e)])
}

module "monitoring" {
  source = "../../modules/monitoring/gcp"
  count  = var.enable_monitoring && var.compute_platform == "cloud-run" ? 1 : 0

  project_id     = var.project_id
  environment    = var.environment
  service_name   = one(module.compute_cloud_run[*].service_name)
  service_url    = trimsuffix(trimprefix(one(module.compute_cloud_run[*].service_uri), "https://"), "/")
  db_instance_id = module.database.instance_name

  alert_email_addresses = local.alert_email_addresses

  labels = local.common_labels
}

# Warns (never blocks) when prod would run with no alert recipient.
check "prod_alerts_reach_someone" {
  assert {
    condition     = !(var.environment == "prod" && (length(module.monitoring) == 0 || length(local.alert_email_addresses) == 0))
    error_message = "environment = \"prod\" has no alert recipient: set enable_monitoring = true (Cloud Run) and the ALERT_EMAILS secret (issue #714)."
  }
}
