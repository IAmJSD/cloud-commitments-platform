variable "project_id" {
  description = "GCP project ID"
  type        = string
}

variable "service_name" {
  description = "Service name"
  type        = string
}

variable "environment" {
  description = "Environment name (dev/staging/prod)"
  type        = string
}

variable "region" {
  description = "GCP region"
  type        = string
}

variable "image_uri" {
  description = "Container image URI from Artifact Registry"
  type        = string
}

variable "cpu" {
  description = "CPU allocation (0.08-8.0, in increments based on memory)"
  type        = string
  default     = "1"
}

variable "memory" {
  description = "Memory allocation (128Mi-32Gi)"
  type        = string
  default     = "512Mi"
}

variable "min_instances" {
  description = "Minimum number of instances"
  type        = number
  default     = 0
}

variable "max_instances" {
  description = "Maximum number of instances"
  type        = number
  default     = 10
}

variable "cpu_throttling" {
  description = "CPU throttling when idle"
  type        = bool
  default     = true
}

variable "startup_cpu_boost" {
  description = "Enable CPU boost during startup"
  type        = bool
  default     = false
}

variable "request_timeout" {
  description = "Request timeout in seconds (1-3600)"
  type        = number
  default     = 300

  validation {
    condition     = var.request_timeout >= 1 && var.request_timeout <= 3600
    error_message = "Request timeout must be between 1 and 3600 seconds."
  }
}

variable "execution_environment" {
  description = "Execution environment (EXECUTION_ENVIRONMENT_GEN1 or EXECUTION_ENVIRONMENT_GEN2)"
  type        = string
  default     = "EXECUTION_ENVIRONMENT_GEN2"
}

variable "ingress" {
  description = "Ingress settings (INGRESS_TRAFFIC_ALL, INGRESS_TRAFFIC_INTERNAL_ONLY, or INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER)"
  type        = string
  default     = "INGRESS_TRAFFIC_ALL"
}

variable "allow_unauthenticated" {
  description = "Allow unauthenticated access"
  type        = bool
  default     = true
}

variable "scheduled_task_auth_mode_override" {
  description = <<-EOT
    Override for SCHEDULED_TASK_AUTH_MODE used ONLY when neither
    var.enable_scheduled_tasks nor var.enable_ri_exchange_schedule is true.
    When either scheduler is enabled, auth mode is always "oidc" -- the
    override is ignored.

    Why this exists (#123): the Cloud Run service can be reachable from the
    open internet independently of the scheduler flags (see
    var.allow_unauthenticated / var.ingress), so tying auth mode to those
    flags would silently boot /api/scheduled/* unauthenticated whenever
    both schedulers are disabled. The fail-closed default ("oidc") means a
    deploy with no scheduler SA still requires the validator to be
    configured with a non-empty subject allow-list, or startup fails
    (internal/server/scheduledauth) instead of serving unauthenticated
    money-path requests. Set this to "disabled" deliberately for
    local-dev / dry-run only.
  EOT
  type        = string
  default     = "oidc"

  validation {
    condition     = contains(["oidc", "bearer", "disabled"], var.scheduled_task_auth_mode_override)
    error_message = "scheduled_task_auth_mode_override must be one of: \"oidc\", \"bearer\", \"disabled\"."
  }
}

variable "database_host" {
  description = "Database host (Cloud SQL private IP)"
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

variable "database_password_secret_id" {
  description = "Secret Manager secret ID containing database password"
  type        = string
}

variable "auto_migrate" {
  description = "Automatically run database migrations on startup"
  type        = bool
  default     = true
}

variable "admin_email" {
  description = "Administrator email address"
  type        = string
}

variable "admin_password_secret_name" {
  description = "Full Secret Manager secret name containing admin password"
  type        = string
  default     = ""
}

variable "enable_admin_password_writer" {
  description = "Grant the Cloud Run SA write access to the admin password secret. Set to true when admin_password_secret_name comes from a resource attribute (known after apply) to avoid sensitive-value-in-count Terraform limitation."
  type        = bool
  default     = false
}

variable "allowed_origins" {
  description = "List of allowed CORS origins"
  type        = list(string)
  default     = []
}

variable "vpc_connector_id" {
  description = "Serverless VPC Access connector ID for Cloud SQL"
  type        = string
  default     = null
}

variable "vpc_egress_mode" {
  description = "VPC egress mode (ALL_TRAFFIC or PRIVATE_RANGES_ONLY)"
  type        = string
  default     = "PRIVATE_RANGES_ONLY"
}

variable "enable_scheduled_tasks" {
  description = "Enable Cloud Scheduler for scheduled tasks"
  type        = bool
  default     = true
}

variable "recommendation_schedule" {
  description = "Cron schedule for recommendations (e.g., '0 2 * * *' for 2 AM daily)"
  type        = string
  default     = "0 2 * * *"
}

# Scheduled server sweeps (#712). Each runs only when enable_scheduled_tasks
# is true, as a Cloud Scheduler job signed with the scheduler SA (see
# scheduled-tasks.tf). Schedules are unix-cron in UTC.
variable "enable_reap_stuck_purchases_schedule" {
  description = "Schedule the reap_stuck_purchases sweep (backstop for purchases stuck in approved/running, #678)."
  type        = bool
  default     = true
}

variable "reap_stuck_purchases_schedule" {
  description = "Cron schedule for reap_stuck_purchases. Keep it more frequent than PURCHASE_APPROVED_REAP_AFTER (default 10m)."
  type        = string
  default     = "*/5 * * * *"
}

variable "enable_analytics_collect_schedule" {
  description = "Schedule the analytics_collect task (savings snapshot, partition maintenance, retention, view refresh)."
  type        = bool
  default     = true
}

variable "analytics_collect_schedule" {
  description = "Cron schedule for analytics_collect."
  type        = string
  default     = "0 4 * * *"
}

variable "enable_fire_scheduled_purchases_schedule" {
  description = "Schedule the fire_scheduled_purchases sweep, which fires delayed auto-approved purchases. Opt-in: default false."
  type        = bool
  default     = false
}

variable "fire_scheduled_purchases_schedule" {
  description = "Cron schedule for fire_scheduled_purchases."
  type        = string
  default     = "*/15 * * * *"
}

variable "enable_process_scheduled_purchases_schedule" {
  description = "Schedule the process_scheduled_purchases sweep (proposes purchases that are due). Opt-in: default false."
  type        = bool
  default     = false
}

variable "process_scheduled_purchases_schedule" {
  description = "Cron schedule for process_scheduled_purchases."
  type        = string
  default     = "0 * * * *"
}

variable "enable_send_notifications_schedule" {
  description = "Schedule the send_notifications sweep. Opt-in: default false; enable it together with process_scheduled_purchases."
  type        = bool
  default     = false
}

variable "send_notifications_schedule" {
  description = "Cron schedule for send_notifications."
  type        = string
  default     = "0 8 * * *"
}

variable "enable_finalize_revocations_schedule" {
  description = "Schedule the finalize_revocations sweep (idempotent audit repair of revoked purchases, plus the RI Marketplace listing poll, which has no schedule of its own: disabling this also stops recording listing sales and proceeds)."
  type        = bool
  default     = true
}

variable "finalize_revocations_schedule" {
  description = "Cron schedule for finalize_revocations."
  type        = string
  default     = "30 * * * *"
}

variable "enable_cleanup_schedule" {
  description = "Schedule the server cleanup sweep. Opt-in: default false. It deletes expired sessions and, via CleanupOldExecutions(30), purchase_executions that are completed with scheduled_date older than 30 days, canceled with updated_at older than 30 days, or in a status outside failed/canceled with expires_at older than 30 days. The first run deletes the whole backlog, so export any history you want to keep first."
  type        = bool
  default     = false
}

variable "cleanup_schedule" {
  description = "Cron schedule for cleanup."
  type        = string
  default     = "0 3 * * *"
}

variable "additional_secret_accessor_ids" {
  description = <<-EOT
    Map of Secret Manager secret IDs (full resource names) the Cloud Run
    service account must be able to read. Each entry produces a per-secret
    `roles/secretmanager.secretAccessor` binding. Map keys are arbitrary
    labels used only for Terraform resource addressing — pick something
    stable so a value change doesn't force a binding recreate.

    Replaces the previous project-wide
    `roles/secretmanager.secretAccessor` grant, which gave Cloud Run read
    access to every secret in the project (overly broad — a typo in any
    other workload's secret name would still be reachable). Pass exactly
    the secrets the workload needs (typically: sendgrid, credential
    encryption key, scheduled-task secret if stored as a Secret Manager
    secret).
  EOT
  type        = map(string)
  default     = {}
}

variable "log_retention_days" {
  description = "Log retention in days (null for default retention)"
  type        = number
  default     = null
}

variable "manage_project_log_retention" {
  description = "Whether to manage the project-level _Default log bucket retention. WARNING: This modifies the project-wide log bucket, affecting all services in the project. Only enable if this module should own project-level log retention."
  type        = bool
  default     = false
}

variable "enable_ri_exchange_schedule" {
  description = "Enable scheduled RI exchange automation"
  type        = bool
  default     = false
}

variable "ri_exchange_schedule" {
  # Standard 5-field cron: 0 */6 * * * runs at 00:00, 06:00, 12:00, 18:00 UTC.
  description = "Cron schedule for RI exchange automation (e.g., '0 */6 * * *' for every 6 hours)"
  type        = string
  default     = "0 */6 * * *"
}

variable "billing_account_id" {
  description = "GCP billing account ID (e.g. '017217-CF7D29-F5AAE4'). Required to grant billing.viewer on the billing account so the app can read SKU prices. Leave empty to skip the billing IAM binding."
  type        = string
  default     = ""
}

variable "additional_env_vars" {
  description = "Additional environment variables"
  type        = map(string)
  default     = {}
}

variable "labels" {
  description = "Labels to apply to resources"
  type        = map(string)
  default     = {}
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

variable "archera_api_key_secret_id" {
  validation {
    condition     = !strcontains(var.archera_api_key_secret_id, "/")
    error_message = "archera_api_key_secret_id must be the short secret ID, not a projects/<number>/secrets/<id> resource name (the per-secret IAM binding sets project and would be replaced on every apply otherwise)."
  }
  description = "Short Secret Manager secret ID (not the projects/<number>/secrets/<id> resource name) holding the Archera API key. Passed as ARCHERA_API_KEY_SECRET and bound with a per-secret secretAccessor grant on this one secret only. Empty disables the comparison."
  type        = string
  default     = ""
}

# Cloud KMS only accepts destroy_scheduled_duration when a key is created:
# the provider marks it ForceNew, so changing it on a deployed key plans a
# replacement of the OIDC signing key. With create_before_destroy that apply
# fails on the key-name collision before the live key is touched (see
# signing-key.tf), so set a longer window only for a project whose key does
# not exist yet. The default keeps every deployed key unchanged (#324).
variable "signing_key_destroy_scheduled_duration" {
  description = "How long the OIDC signing key's versions stay in DESTROY_SCHEDULED (restorable) before Cloud KMS destroys them, as \"<seconds>s\". Between 1 day (86400s) and 120 days (10368000s). Only takes effect when the key is created; see the comment above."
  type        = string
  default     = "86400s"

  validation {
    condition = (
      can(regex("^[0-9]+s$", var.signing_key_destroy_scheduled_duration)) &&
      try(tonumber(trimsuffix(var.signing_key_destroy_scheduled_duration, "s")) >= 86400, false) &&
      try(tonumber(trimsuffix(var.signing_key_destroy_scheduled_duration, "s")) <= 10368000, false)
    )
    error_message = "signing_key_destroy_scheduled_duration must be a whole number of seconds with an \"s\" suffix, between \"86400s\" (1 day) and \"10368000s\" (120 days), the range Cloud KMS accepts."
  }
}
