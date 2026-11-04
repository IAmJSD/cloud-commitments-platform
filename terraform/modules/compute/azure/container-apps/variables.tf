variable "app_name" {
  description = "Application name"
  type        = string
}

variable "environment" {
  description = "Environment name (dev/staging/prod)"
  type        = string
}

variable "resource_group_name" {
  description = "Resource group name"
  type        = string
}

variable "location" {
  description = "Azure region"
  type        = string
}

variable "image_uri" {
  description = "Container image URI"
  type        = string
}

variable "cpu" {
  description = "CPU allocation (0.25, 0.5, 0.75, 1.0, 1.25, 1.5, 1.75, 2.0)"
  type        = number
  default     = 0.5

  validation {
    condition     = contains([0.25, 0.5, 0.75, 1.0, 1.25, 1.5, 1.75, 2.0], var.cpu)
    error_message = "CPU must be one of: 0.25, 0.5, 0.75, 1.0, 1.25, 1.5, 1.75, 2.0"
  }
}

variable "memory" {
  description = "Memory allocation (0.5Gi, 1.0Gi, 1.5Gi, 2.0Gi, 3.0Gi, 4.0Gi)"
  type        = string
  default     = "1.0Gi"

  validation {
    condition     = contains(["0.5Gi", "1.0Gi", "1.5Gi", "2.0Gi", "3.0Gi", "4.0Gi"], var.memory)
    error_message = "Memory must be one of: 0.5Gi, 1.0Gi, 1.5Gi, 2.0Gi, 3.0Gi, 4.0Gi"
  }
}

variable "min_replicas" {
  description = "Minimum number of replicas"
  type        = number
  default     = 0
}

variable "max_replicas" {
  description = "Maximum number of replicas"
  type        = number
  default     = 10
}

variable "external_ingress_enabled" {
  description = "Enable external ingress (public access)"
  type        = bool
  default     = true
}

variable "infrastructure_subnet_id" {
  description = "Subnet ID for Container App Environment infrastructure"
  type        = string
  default     = null
}

variable "internal_load_balancer_enabled" {
  description = "Enable internal load balancer"
  type        = bool
  default     = false
}

variable "log_analytics_workspace_id" {
  description = "Log Analytics workspace ID for Container App Environment"
  type        = string
  default     = null
}

variable "enable_diagnostics" {
  description = "Enable diagnostic settings for Container App"
  type        = bool
  default     = false
}

variable "database_host" {
  description = "Database host (FQDN)"
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

variable "database_password_secret_name" {
  description = "Key Vault secret name for database password"
  type        = string
}

variable "subscription_id" {
  description = "Host subscription GUID. Supplied by the caller rather than read from data.azurerm_subscription so every RBAC scope in this module stays known at plan time; see the RBAC section of main.tf."
  type        = string

  # Input validation, not a security control: a GUID-shaped string is accepted
  # whether or not that subscription exists, is reachable, or is the right one.
  # What it buys is a clear plan-time message for a value that is not a GUID at
  # all (a subscription display name, say), which would otherwise surface as an
  # opaque Azure rejection of a malformed RBAC scope far from the cause.
  validation {
    condition     = can(regex("^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$", var.subscription_id))
    error_message = "The subscription_id must be an Azure subscription GUID in 8-4-4-4-12 hexadecimal form, upper or lower case."
  }
}

variable "reservation_role_definition_id" {
  description = "Full ARM resource ID of the bootstrap-created custom reservation-purchaser role definition (terraform/modules/iam/azure/cudly-reservation-role, output role_definition_resource_id). Looked up by the caller so it is known at plan time."
  type        = string
}

variable "key_vault_uri" {
  description = "Key Vault URI (data-plane, e.g. https://<name>.vault.azure.net/)"
  type        = string
}

variable "key_vault_id" {
  description = "Key Vault ARM resource ID (/subscriptions/.../providers/Microsoft.KeyVault/vaults/<name>) used as the RBAC scope for the OIDC signing-key Crypto User role."
  type        = string
}

variable "signing_key_name" {
  description = "Name of the OIDC issuer signing key in Key Vault"
  type        = string
}

variable "auto_migrate" {
  description = "Auto-run database migrations on startup"
  type        = bool
  default     = true
}

variable "admin_email" {
  description = "Administrator email address"
  type        = string
}

variable "admin_password_secret_name" {
  description = "Key Vault secret name containing admin password. When set together with admin_email, the runtime identity gets write access to this one secret for the admin-password sync (#199)."
  type        = string
  default     = ""

  # The write grant's RBAC scope is <key_vault_id>/secrets/<this name>, so a
  # value carrying a "/" (or anything else outside Key Vault's secret-name
  # alphabet) could move that scope somewhere other than exactly one secret.
  validation {
    condition     = var.admin_password_secret_name == "" || can(regex("^[0-9A-Za-z-]{1,127}$", var.admin_password_secret_name))
    error_message = "admin_password_secret_name must be empty or a bare Key Vault secret name (1-127 letters, digits and hyphens), not a path or ID."
  }
}

variable "allowed_origins" {
  description = "List of allowed CORS origins"
  type        = list(string)
  default     = []
}

variable "additional_env_vars" {
  description = "Additional environment variables"
  type        = map(string)
  default     = {}
}

variable "secrets" {
  description = "List of secrets to mount from Key Vault"
  type = list(object({
    name  = string
    value = string
  }))
  default = []
}

variable "custom_domains" {
  description = "List of custom domains"
  type = list(object({
    name           = string
    certificate_id = string
  }))
  default = []
}

variable "registry_server" {
  description = "Container registry server URL (e.g. myacr.azurecr.io)"
  type        = string
}

variable "container_registry_id" {
  description = "Container registry ARM resource ID, the scope of the AcrPull grant to the app's managed identity"
  type        = string
}

variable "tags" {
  description = "Tags to apply to resources"
  type        = map(string)
  default     = {}
}

# ==============================================
# Scheduled Tasks (Logic Apps) Configuration
# ==============================================

variable "enable_scheduled_tasks" {
  description = "Enable scheduled tasks via Logic Apps"
  type        = bool
  default     = true
}

variable "scheduled_task_secret_name" {
  description = "Key Vault secret name (NOT the value) holding the shared secret for authenticating scheduled task HTTP calls. The Logic App workflows fetch this secret at runtime via their managed identity, so the plaintext never lands in the workflow definition or Terraform state. Must be non-empty when enable_scheduled_tasks or enable_ri_exchange_schedule is true (enforced by a precondition in scheduled-tasks.tf)."
  type        = string
  default     = ""

  # Validation runs unconditionally and can't reference sibling variables, so
  # the "non-empty when scheduled tasks are enabled" rule lives in the
  # `terraform_data.scheduled_task_secret_preconditions` resource in
  # scheduled-tasks.tf. This block handles only the character-set half: if a
  # value is supplied, it must be a bare Key Vault secret name — no `/`, `?`,
  # `#`, or whitespace — so it can't escape the
  # `${key_vault_uri}secrets/${name}?api-version=…` URL template.
  validation {
    condition     = var.scheduled_task_secret_name == "" || !can(regex("[/?# ]", var.scheduled_task_secret_name))
    error_message = "scheduled_task_secret_name must be a bare Key Vault secret name (no '/', '?', '#', or whitespace)."
  }
}

variable "recommendation_schedule" {
  description = <<-EOT
    Cron-shaped schedule for the recommendations Logic App. Only the
    hour field (index [1]) is parsed — see scheduled-tasks.tf — and
    the value is materialized as a frequency=Day + interval=1
    recurrence starting at that hour. Sub-daily, day-of-week, and
    day-of-month cron patterns are silently ignored; stick to
    daily-at-hour-N values. Default: daily at 02:00 UTC.
  EOT
  type        = string
  default     = "0 2 * * *"
}

# Scheduled server sweeps (#712). Each runs only when enable_scheduled_tasks
# is true, as a Logic App workflow (see scheduled-tasks.tf). Schedules are
# 5-field cron in UTC, limited to the shapes a Logic Apps recurrence can
# express exactly: "*/N * * * *", "M * * * *", "M */N * * *" and
# "M H * * *". Anything else fails the plan. cleanup keeps its existing
# workflow under enable_scheduled_tasks and has no flag of its own here.
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
  description = "Schedule the fire_scheduled_purchases sweep, which carries out every purchase a person approves while a purchase delay is set (purchase_delay_hours > 0; the migrated default is 0, which executes approvals immediately) once the delay ends. With the sweep off, those approved purchases stay scheduled and never execute, with no alert, so turn it off only where purchase_delay_hours stays 0. On by default: the server refuses, and alerts on, any purchase due more than 24h ago (SCHEDULED_PURCHASE_MAX_OVERDUE, #1549), so turning it on cannot buy a stale backlog. Set false to opt out."
  type        = bool
  default     = true
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

variable "enable_ri_exchange_schedule" {
  description = "Enable scheduled RI exchange automation via Logic Apps"
  type        = bool
  default     = false
}

variable "ri_exchange_schedule" {
  # Standard 5-field cron: 0 */6 * * * runs at 00:00, 06:00, 12:00, 18:00 UTC.
  description = "Cron schedule for RI exchange automation (e.g., '0 */6 * * *' for every 6 hours)"
  type        = string
  default     = "0 */6 * * *"
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

variable "archera_api_key_secret_name" {
  description = "Name of the Key Vault secret holding the Archera API key, passed as ARCHERA_API_KEY_SECRET. The secret lives in the platform vault, where the runtime identity already holds the vault-wide Key Vault Secrets User role, so no role assignment is added here. Empty disables the comparison."
  type        = string
  default     = ""
}
