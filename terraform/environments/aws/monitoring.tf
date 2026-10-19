# ==============================================
# Monitoring and failure alerts (issue #714)
# ==============================================
#
# Instantiates modules/monitoring/aws: an SNS alerts topic with one email
# subscription per address in var.alert_emails, CloudWatch alarms on the
# Lambda function and the RDS instance, and log-metric alarms on any failed
# purchase execution or failed scheduled task.
#
# Off by default, like modules/compute/aws/lambda's migration alarm: the SNS
# topic, dashboard, metric filters and query definitions need deploy-role
# grants (sns:*, cloudwatch:PutDashboard, logs:PutMetricFilter,
# logs:PutQueryDefinition) that only exist once the ci-cd-permissions
# bootstrap has been re-applied (root CLAUDE.md "CI/CD IAM" split). Turning
# this on first would 403 every apply. To enable:
#   1. re-apply terraform/environments/aws/ci-cd-permissions,
#   2. set the ALERT_EMAILS repository secret (comma-separated addresses),
#   3. set enable_monitoring = true in the environment's tfvars.
# Each address must confirm the SNS subscription email before alerts arrive.

variable "enable_monitoring" {
  description = "Create the monitoring module (SNS alerts topic, alarms, dashboard, log metric filters). Requires the ci-cd-permissions bootstrap to have been re-applied with the #714 grants; see monitoring.tf."
  type        = bool
  default     = false
}

variable "alert_emails" {
  description = "Comma-separated email addresses subscribed to the alerts SNS topic. Supplied by the deploy workflow from the ALERT_EMAILS secret; never commit real addresses. Empty means alarms fire with nobody subscribed (the prod check below warns)."
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
  source = "../../modules/monitoring/aws"
  count  = var.enable_monitoring ? 1 : 0

  stack_name       = local.stack_name
  environment      = var.environment
  aws_region       = var.region
  compute_platform = var.compute_platform

  function_name = var.compute_platform == "lambda" ? one(module.compute_lambda[*].function_name) : ""
  log_group_name = (
    var.compute_platform == "lambda"
    ? one(module.compute_lambda[*].log_group_name)
    : one(module.compute_fargate[*].log_group_name)
  )
  db_instance_identifier = module.database.instance_identifier

  alert_email_addresses = local.alert_email_addresses

  tags = local.common_tags
}

# Warns (never blocks) when prod would run with no alert recipient. Same
# check-not-precondition reasoning as checks.tf: this must not stop an
# unrelated prod deploy.
check "prod_alerts_reach_someone" {
  assert {
    condition     = !(var.environment == "prod" && (!var.enable_monitoring || length(local.alert_email_addresses) == 0))
    error_message = "environment = \"prod\" has no alert recipient: set enable_monitoring = true and the ALERT_EMAILS secret (issue #714)."
  }
}

output "alerts_sns_topic_arn" {
  description = "SNS topic the monitoring alarms notify (null when enable_monitoring is false)"
  value       = one(module.monitoring[*].sns_topic_arn)
}
