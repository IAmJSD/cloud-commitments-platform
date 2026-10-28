# AWS Fargate Module Variables

variable "stack_name" {
  description = "Name of the stack"
  type        = string
}

variable "permissions_boundary_arn" {
  description = <<-EOT
    ARN of the permissions boundary that every IAM role in this module must
    carry. Required, not optional: cudly-terraform-deploy's IAM grants are
    conditioned on iam:PermissionsBoundary, so a role created without this
    boundary cannot have its inline policies or managed-policy attachments
    written and the apply fails with AccessDenied. See
    terraform/environments/aws/ci-cd-permissions/policy_boundary.tf.
  EOT
  type        = string
}

variable "environment" {
  description = "Environment name (dev/staging/prod)"
  type        = string
}

variable "region" {
  description = "AWS region"
  type        = string
}

variable "image_uri" {
  description = "ECR image URI for Fargate container"
  type        = string
}

variable "cpu_architecture" {
  description = "ECS task CPU architecture (arm64 or x86_64)"
  type        = string
  default     = "x86_64"

  validation {
    condition     = contains(["arm64", "x86_64"], var.cpu_architecture)
    error_message = "cpu_architecture must be 'arm64' or 'x86_64'."
  }
}

variable "cpu" {
  description = "Fargate CPU units (256, 512, 1024, 2048, 4096)"
  type        = number
  default     = 512
}

variable "memory" {
  description = "Fargate memory in MB (512, 1024, 2048, 4096, 8192, 16384, 30720)"
  type        = number
  default     = 1024
}

variable "desired_count" {
  description = "Desired number of tasks"
  type        = number
  default     = 2
}

variable "min_capacity" {
  description = "Minimum number of tasks for auto-scaling"
  type        = number
  default     = 1
}

variable "max_capacity" {
  description = "Maximum number of tasks for auto-scaling"
  type        = number
  default     = 10
}

variable "database_host" {
  description = "Database endpoint"
  type        = string
}

variable "database_name" {
  description = "Database name"
  type        = string
}

variable "database_username" {
  description = "Database username"
  type        = string
}

variable "database_password_secret_arn" {
  description = "ARN of Secrets Manager secret containing database password"
  type        = string
}

variable "admin_email" {
  description = "Email address for the default admin user (created without password - must use password reset)"
  type        = string
}

variable "admin_password_secret_arn" {
  description = "ARN of Secrets Manager secret containing admin password"
  type        = string
  default     = ""
}

variable "auto_migrate" {
  description = "Automatically run database migrations on startup"
  type        = bool
  default     = true
}

variable "vpc_id" {
  description = "VPC ID"
  type        = string
}

variable "private_subnet_ids" {
  description = "Private subnet IDs for ECS tasks"
  type        = list(string)
}

variable "public_subnet_ids" {
  description = "Public subnet IDs for Application Load Balancer"
  type        = list(string)
}

variable "alb_security_group_id" {
  description = "Security group ID for ALB"
  type        = string
}

variable "enable_https" {
  description = "Enable HTTPS listener on ALB"
  type        = bool
  default     = true
}

variable "certificate_arn" {
  description = "ARN of ACM certificate for HTTPS"
  type        = string
  default     = ""
}

variable "health_check_path" {
  description = "ALB target group health check path. This check admits traffic, so it must be the readiness endpoint (/ready answers 503 until the task has finished initializing, #647); /health answers 200 even while degraded and is liveness only."
  type        = string
  default     = "/ready"
}

variable "health_check_grace_period_seconds" {
  description = "Seconds ECS ignores failing ALB health checks on a new task before replacing it. The ALB keeps an unready task out of rotation either way; this only stops ECS from killing a task whose database initialization (connect retries plus migrations) is still running."
  type        = number
  default     = 300

  validation {
    condition     = var.health_check_grace_period_seconds >= 0 && var.health_check_grace_period_seconds <= 2147483647 && floor(var.health_check_grace_period_seconds) == var.health_check_grace_period_seconds
    error_message = "health_check_grace_period_seconds must be a whole number between 0 and 2147483647."
  }
}

variable "health_check_interval" {
  description = "Health check interval in seconds"
  type        = number
  default     = 30
}

variable "health_check_timeout" {
  description = "Health check timeout in seconds"
  type        = number
  default     = 5
}

variable "healthy_threshold" {
  description = "Number of consecutive successful health checks"
  type        = number
  default     = 2
}

variable "unhealthy_threshold" {
  description = "Consecutive failed ALB health checks before a target is marked unhealthy. The check is readiness (/ready), which also answers 503 while the database is unreachable, and ECS replaces targets the ALB marks unhealthy, so this is set high enough that a short database blip or a Multi-AZ failover does not cycle every task: with the default 30s interval, 6 failures is about 180s of continuous 503 before replacement starts."
  type        = number
  default     = 6
}

variable "log_retention_days" {
  description = "CloudWatch log retention in days"
  type        = number
  default     = 90
}

variable "enable_scheduled_tasks" {
  description = "Enable scheduled EventBridge tasks"
  type        = bool
  default     = true
}

variable "recommendation_schedule" {
  description = "EventBridge schedule expression for recommendation collection. Default: daily at 02:00 UTC, the same time as GCP and Azure. A cron() schedule lets the API report next_collection_at; a rate() schedule leaves it null because its phase is unknown (#756)."
  type        = string
  default     = "cron(0 2 * * ? *)"
}

variable "enable_ri_exchange_schedule" {
  description = "Enable scheduled RI exchange automation"
  type        = bool
  default     = false
}

variable "ri_exchange_schedule" {
  description = "Schedule for RI exchange automation. rate() starts from deployment time; cron(0 0/6 * * ? *) runs at fixed clock hours (EventBridge cron syntax). For predictable approval windows, prefer cron."
  type        = string
  default     = "rate(6 hours)"
}

variable "enable_ladder_run_schedule" {
  description = "Enable the scheduled commitment-ladder planning run. Default false until laddering is promoted to GA."
  type        = bool
  default     = false
}

variable "ladder_run_schedule" {
  description = "EventBridge schedule for the commitment-ladder planning run (e.g. rate(1 day) or cron(0 2 * * ? *) for 02:00 UTC)."
  type        = string
  default     = "rate(1 day)"
}

variable "enable_fire_scheduled_purchases_schedule" {
  description = "Schedule the fire_scheduled_purchases sweep, which carries out every purchase a person approves while a purchase delay is set (purchase_delay_hours > 0; the migrated default is 0, which executes approvals immediately) once the delay ends. With the sweep off, those approved purchases stay scheduled and never execute, with no alert, so turn it off only where purchase_delay_hours stays 0. On by default: the server refuses, and alerts on, any purchase due more than 24h ago (SCHEDULED_PURCHASE_MAX_OVERDUE, #1549), so turning it on cannot buy a stale backlog. Set false to opt out."
  type        = bool
  default     = true
}

variable "fire_scheduled_purchases_schedule" {
  description = "EventBridge schedule for the fire_scheduled_purchases sweep (default rate(15 minutes))."
  type        = string
  default     = "rate(15 minutes)"
}

# Sweeps in sweeps.tf. Names, defaults and opt-in policy match the Lambda module.

variable "enable_reap_stuck_purchases_schedule" {
  description = "Enable scheduled sweep of purchase_executions stuck in approved/running. Issue #678 backstop for executor crashes."
  type        = bool
  default     = true
}

variable "reap_stuck_purchases_schedule" {
  description = "EventBridge schedule for the stuck-purchase reaper. Should be more frequent than PURCHASE_APPROVED_REAP_AFTER (default 10m) so a stuck row is reaped within ~1 threshold-window."
  type        = string
  default     = "rate(5 minutes)"
}

variable "enable_analytics_collect_schedule" {
  description = "Enable the scheduled savings-snapshot analytics collection (issues #1023/#1033): snapshot, partition maintenance, retention and view refresh."
  type        = bool
  default     = true
}

variable "analytics_collect_schedule" {
  description = "EventBridge schedule for the analytics_collect task. rate() starts from deployment time; use cron() for fixed clock times."
  type        = string
  default     = "rate(1 day)"
}

variable "enable_process_scheduled_purchases_schedule" {
  description = "Enable the scheduled process_scheduled_purchases sweep (proposes purchases that are due). Opt-in: default false so no purchase proposal runs unless an operator asks for it."
  type        = bool
  default     = false
}

variable "process_scheduled_purchases_schedule" {
  description = "EventBridge schedule for the process_scheduled_purchases sweep."
  type        = string
  default     = "rate(1 hour)"
}

variable "enable_send_notifications_schedule" {
  description = "Enable the scheduled send_notifications sweep. Opt-in: default false; enable it together with process_scheduled_purchases."
  type        = bool
  default     = false
}

variable "send_notifications_schedule" {
  description = "EventBridge schedule for the send_notifications sweep."
  type        = string
  default     = "rate(1 day)"
}

variable "enable_finalize_revocations_schedule" {
  description = "Enable the scheduled finalize_revocations sweep (idempotent audit repair of revoked purchases, plus the RI Marketplace listing poll, which has no schedule of its own: disabling this also stops recording listing sales and proceeds)."
  type        = bool
  default     = true
}

variable "finalize_revocations_schedule" {
  description = "EventBridge schedule for the finalize_revocations sweep."
  type        = string
  default     = "rate(1 hour)"
}

variable "enable_cleanup_schedule" {
  description = "Enable the scheduled server cleanup sweep. Opt-in: default false. It deletes expired sessions and, via CleanupOldExecutions(30), purchase_executions that are completed with scheduled_date older than 30 days, canceled with updated_at older than 30 days, or in a status outside failed/canceled with expires_at older than 30 days. The first run deletes the whole backlog, so export any history you want to keep first."
  type        = bool
  default     = false
}

variable "cleanup_schedule" {
  description = "EventBridge schedule for the cleanup sweep."
  type        = string
  default     = "rate(1 day)"
}

variable "task_timeout" {
  description = "Timeout in seconds for one-off scheduled tasks"
  type        = number
  default     = 900
}

variable "additional_env_vars" {
  description = "Additional environment variables"
  type        = map(string)
  default     = {}
}

variable "allowed_origins" {
  description = "CORS allowed origins"
  type        = list(string)
  default     = []
}

variable "enable_execute_command" {
  description = "Enable ECS Exec for debugging"
  type        = bool
  default     = false
}

variable "enable_container_insights" {
  description = "Enable ECS Container Insights (CloudWatch custom metrics per container). Has per-metric cost implications (~$0.30/metric/month). Recommended for production."
  type        = bool
  default     = false
}

variable "enable_alb_deletion_protection" {
  description = "Enable ALB deletion protection (recommended for production)"
  type        = bool
  default     = false
}

variable "tags" {
  description = "Additional tags for resources"
  type        = map(string)
  default     = {}
}

# ==============================================
# Multi-account credentials + discovery
# ==============================================
# These options mirror the Lambda module's 5-field shape so the Fargate
# path is at feature parity. See terraform/modules/compute/aws/lambda/variables.tf
# for the same set.

variable "credential_encryption_key_secret_arn" {
  description = "ARN of the Secrets Manager secret holding the credential encryption key. Empty string disables the GetSecretValue grant on this resource."
  type        = string
  default     = ""
}

variable "scheduled_task_secret_arn" {
  description = "ARN of Secrets Manager secret holding the bearer secret for /api/scheduled/* (required when SCHEDULED_TASK_AUTH_MODE=bearer; resolved at runtime via SecretResolver)"
  type        = string
  default     = ""
}

variable "scheduled_task_secret_name" {
  description = "Name of the scheduled-task bearer secret. Passed to compute as SCHEDULED_TASK_SECRET_NAME and resolved by internal/server.resolveScheduledTaskSecret on cold start."
  type        = string
  default     = ""
}

variable "enable_cross_account_sts" {
  description = "Grant the task role sts:AssumeRole on IAM roles whose name starts with cross_account_role_name_prefix. Required for multi-account plan execution."
  type        = bool
  default     = true
}

variable "cross_account_role_name_prefix" {
  description = "Prefix that scopes the cross_account_sts grant. The shipped federation templates (iac/federation/aws-*) create roles starting with this prefix."
  type        = string
  default     = "CUDly"
}

variable "cross_account_target_account_ids" {
  description = "AWS account IDs the task role itself calls sts:AssumeRole against. Rendered into an aws:ResourceAccount condition on the grant, so an account not listed here is denied by IAM rather than by application-layer account selection (#1636). For role_arn accounts this is the target account. For bastion accounts it is the BASTION's account, not the target's: the task assumes into the bastion, and the bastion assumes into the target on its own identity policy, which this condition does not govern (internal/credentials/resolver.go:242-247). Must be non-empty when enable_cross_account_sts is true (enforced by a precondition in main.tf); an empty list means no cross-account reach, never all accounts."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for id in var.cross_account_target_account_ids : can(regex("^[0-9]{12}$", id))])
    error_message = "cross_account_target_account_ids must contain bare 12-digit AWS account IDs. A wildcard is not an account ID: the condition uses StringEquals, under which '*' is the literal string '*' and matches nothing, so it denies every cross-account call rather than allowing them. There is no 'any account' value."
  }
}

variable "enable_org_discovery" {
  description = "Grant the task role organizations:ListAccounts so CUDly can enumerate accounts in an AWS Organizations management / delegated account."
  type        = bool
  default     = false
}

variable "email_from_domain" {
  description = "Verified SES From domain. When set, the task role gets SES send permissions scoped to this domain plus stack-specific configuration sets. Leave empty to disable SES entirely."
  type        = string
  default     = ""
}

# Archera insured-commitment comparison (default off; see the Archera section of docs/DEPLOYMENT.md).
# All three must be set for the feature to be configured; the key itself is never
# managed here, the operator creates the secret out-of-band.
variable "archera_org_id" {
  description = "Archera organization UUID, passed as ARCHERA_ORG_ID. Empty disables the comparison."
  type        = string
  default     = ""
}

variable "archera_plan_id" {
  description = "Archera commitment plan UUID, passed as ARCHERA_PLAN_ID. Empty disables the comparison."
  type        = string
  default     = ""
}

variable "archera_api_key_secret_arn" {
  description = "ARN of the Secrets Manager secret holding the Archera API key. Passed as ARCHERA_API_KEY_SECRET and granted GetSecretValue on this one secret only. Empty disables the comparison."
  type        = string
  default     = ""
}
