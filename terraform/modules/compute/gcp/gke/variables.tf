# GCP GKE Module Variables

variable "project_name" {
  description = "Project name for resource naming"
  type        = string
}

variable "environment" {
  description = "Environment name (dev/staging/prod)"
  type        = string
}

variable "project_id" {
  description = "GCP project ID"
  type        = string
}

variable "region" {
  description = "GCP region"
  type        = string
}

variable "zones" {
  description = "GCP zones for node pools"
  type        = list(string)
  default     = []
}

variable "network_name" {
  description = "VPC network name"
  type        = string
}

variable "subnetwork_name" {
  description = "VPC subnetwork name"
  type        = string
}

variable "image_name" {
  description = "Container image name (with registry)"
  type        = string
}

variable "image_tag" {
  description = "Container image tag"
  type        = string
  default     = "latest"
}

variable "kubernetes_version" {
  description = "Kubernetes version"
  type        = string
  default     = "1.30"
}

variable "node_count" {
  description = "Initial number of nodes per zone"
  type        = number
  default     = 1
}

variable "node_machine_type" {
  description = "Machine type for nodes"
  type        = string
  default     = "e2-standard-2"
}

variable "node_disk_size_gb" {
  description = "Disk size for nodes in GB"
  type        = number
  default     = 50
}

variable "min_node_count" {
  description = "Minimum node count for auto-scaling (per zone)"
  type        = number
  default     = 1
}

variable "max_node_count" {
  description = "Maximum node count for auto-scaling (per zone)"
  type        = number
  default     = 3
}

variable "database_host" {
  description = "Cloud SQL instance connection name"
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
  description = "Name of Secret Manager secret containing database password"
  type        = string
}

variable "secret_manager_project_id" {
  description = "GCP project ID for Secret Manager (defaults to main project)"
  type        = string
  default     = ""
}

variable "additional_secret_accessor_ids" {
  description = <<-EOT
    Map of Secret Manager secret IDs (full resource names) the GKE workload
    Identity service account must be able to read. Each entry produces a
    per-secret `roles/secretmanager.secretAccessor` binding. Map keys are
    arbitrary labels used only for Terraform resource addressing — pick
    something stable so a value change doesn't force a binding recreate.

    Previously the GKE workload SA only had access to
    `database_password_secret_name`, which meant any code path reading
    other secrets (credential encryption key, sendgrid API key, etc.)
    silently failed at runtime. Wire the relevant secret IDs through
    here so the bindings exist before the first pod starts.
  EOT
  type        = map(string)
  default     = {}
}

variable "enable_workload_identity" {
  description = "Enable Workload Identity for GKE"
  type        = bool
  default     = true
}

variable "enable_auto_scaling" {
  description = "Enable cluster auto-scaling"
  type        = bool
  default     = true
}

variable "enable_auto_repair" {
  description = "Enable automatic node repair"
  type        = bool
  default     = true
}

variable "enable_auto_upgrade" {
  description = "Enable automatic node upgrades"
  type        = bool
  default     = true
}

variable "enable_http_load_balancing" {
  description = "Enable HTTP Load Balancing add-on"
  type        = bool
  default     = true
}

variable "enable_horizontal_pod_autoscaling" {
  description = "Enable Horizontal Pod Autoscaling add-on"
  type        = bool
  default     = true
}

variable "labels" {
  description = "Additional labels for resources"
  type        = map(string)
  default     = {}
}

variable "admin_email" {
  description = "Administrator email address"
  type        = string
  default     = ""
}

variable "admin_password_secret_name" {
  description = "Full Secret Manager secret name containing admin password"
  type        = string
  default     = ""
}

variable "enable_admin_password_writer" {
  description = "Grant the workload SA read and version-add access to the admin password secret. Set to true when admin_password_secret_name comes from a resource attribute (known after apply) to avoid sensitive-value-in-count Terraform limitation."
  type        = bool
  default     = false
}

variable "auto_migrate" {
  description = "Automatically run database migrations on startup"
  type        = bool
  default     = true
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

variable "deploy_kubernetes_resources" {
  description = "Deploy kubernetes resources (namespace, deployment, service, etc). Requires kubernetes/helm providers to be configured at root level. Set to false to only create the GKE cluster."
  type        = bool
  default     = false
}

# ==============================================
# Health Check Configuration
# ==============================================

variable "health_check_path" {
  description = "HTTP path for the liveness probe. /health answers 200 while the process is alive, even before initialization completes, so a slow init does not get the pod restarted."
  type        = string
  default     = "/health"
}

variable "readiness_probe_path" {
  description = "HTTP path for the readiness probe (traffic admission). /ready answers 503 until the replica has completed initialization, keeping cold pods out of the Service endpoints (#648)."
  type        = string
  default     = "/ready"

  validation {
    condition     = startswith(var.readiness_probe_path, "/")
    error_message = "readiness_probe_path must be an absolute HTTP path starting with \"/\"."
  }
}

variable "health_check_port" {
  description = "Port for health checks"
  type        = number
  default     = 8080
}

variable "liveness_probe_initial_delay" {
  description = "Liveness probe initial delay in seconds"
  type        = number
  default     = 30
}

variable "liveness_probe_period" {
  description = "Liveness probe period in seconds"
  type        = number
  default     = 10
}

variable "readiness_probe_initial_delay" {
  description = "Readiness probe initial delay in seconds"
  type        = number
  default     = 10
}

variable "readiness_probe_period" {
  description = "Readiness probe period in seconds"
  type        = number
  default     = 5
}

# ==============================================
# Scheduled Tasks Configuration
# ==============================================

variable "enable_scheduled_tasks" {
  description = "Enable Cloud Scheduler for scheduled tasks"
  type        = bool
  default     = false
}

variable "recommendation_schedule" {
  description = "Cron schedule for recommendations (e.g., '0 2 * * *' for 2 AM daily)"
  type        = string
  default     = "0 2 * * *"
}

# Scheduled server sweeps (#712). Each runs only when enable_scheduled_tasks
# is true (app_url must then be set), as a Cloud Scheduler job signed with the
# scheduler SA (scheduled-tasks.tf). Schedules are unix-cron in UTC.
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

variable "app_url" {
  description = "Application URL for scheduled task HTTP triggers (e.g., https://app.example.com). Required when enable_scheduled_tasks is true."
  type        = string
  default     = ""
}

variable "scheduled_task_auth_mode_override" {
  description = <<-EOT
    Override for SCHEDULED_TASK_AUTH_MODE used ONLY when no scheduler SA
    is created (var.enable_scheduled_tasks = false). When the SA exists,
    auth mode is always derived as "oidc" — the override is ignored.

    Why this exists: kubernetes_ingress_v1.app exposes /api/scheduled/*
    via the catch-all rule even when the scheduler is disabled. Tying
    auth to var.enable_scheduled_tasks would silently boot those
    endpoints unauthenticated. The fail-closed default ("oidc") here
    means a deploy without scheduler SA still requires the validator to
    be configured (or it rejects every request). Set this to "disabled"
    deliberately for local-dev / dry-run only.
  EOT
  type        = string
  default     = "oidc"

  validation {
    condition     = contains(["oidc", "bearer", "disabled"], var.scheduled_task_auth_mode_override)
    error_message = "scheduled_task_auth_mode_override must be one of: \"oidc\", \"bearer\", \"disabled\"."
  }
}

variable "enable_private_nodes" {
  description = "Give GKE nodes private IPs only (no public node IPs). Defaults to true; there is no legitimate reason to expose node IPs publicly."
  type        = bool
  default     = true
}

variable "enable_private_endpoint" {
  description = "Remove the GKE control plane's public IP entirely (kubectl then requires VPC peering, Private Service Connect, or a bastion inside the network). Defaults to false: the public endpoint stays present but is denied to every external IP by default (see var.master_authorized_networks and var.gcp_public_cidrs_access_enabled) rather than reachable from 0.0.0.0/0. CONSEQUENCE either way: with the default empty var.master_authorized_networks, a standard GitHub-hosted runner (ubuntu-latest) cannot reach the public endpoint at all -- ubuntu-latest has no stable, allowlistable egress CIDR. terraform apply itself (creating/updating the cluster) does not need API server reachability, but var.deploy_kubernetes_resources=true (which configures kubernetes/helm providers against this cluster) does, and needs either a self-hosted/VNet-reachable runner or a real entry in var.master_authorized_networks."
  type        = bool
  default     = false
}

variable "master_ipv4_cidr_block" {
  description = <<-EOT
    /28 CIDR for the GKE control plane's own VPC (used for its private peering
    endpoint). Required by the google provider whenever enable_private_nodes =
    true on a Standard cluster and neither this nor
    private_endpoint_subnetwork is set -- terraform validate and plan do not
    catch the omission (it is a provider-side check at apply/create), but
    apply fails outright with "master_ipv4_cidr_block or
    private_endpoint_subnetwork is required".

    Must not overlap var.network_name / var.subnetwork_name's ranges. The
    default is a distinct RFC1918 /16 (172.16.0.0/16) from the 10.0.0.0/8
    space terraform/environments/gcp's subnet_cidr (default 10.0.0.0/24) and
    connector_subnet_cidr (default 10.8.0.0/28) use, so it cannot collide
    with this repo's default networking layout; an operator using a
    non-default VPC CIDR plan must override this to a /28 outside it.
  EOT
  type        = string
  default     = "172.16.0.0/28"

  validation {
    condition     = can(cidrnetmask(var.master_ipv4_cidr_block)) && tonumber(split("/", var.master_ipv4_cidr_block)[1]) == 28
    error_message = "master_ipv4_cidr_block must be a valid /28 CIDR (e.g. 172.16.0.0/28); GKE requires exactly a /28 for the control plane's private peering range."
  }
}

variable "gcp_public_cidrs_access_enabled" {
  description = <<-EOT
    Whether Google Cloud's own public IP ranges (used by services like Cloud
    Build) can reach the GKE control plane's public endpoint, independent of
    var.master_authorized_networks. Defaults to false: leaving this at the
    provider's own default (true) would mean an empty
    master_authorized_networks allowlist does NOT actually deny all external
    access -- any Google Cloud public IP could still reach the endpoint. Set
    to true only if a Google-managed service genuinely needs direct API
    server access (most integrations use the GKE API instead and do not need
    this).
  EOT
  type        = bool
  default     = false
}

variable "master_authorized_networks" {
  description = "CIDR allowlist for the GKE control plane's public endpoint (CI egress ranges, an operator bastion). Defaults to empty; combined with gcp_public_cidrs_access_enabled=false (also this module's default), that denies all external access -- only in-VPC/private traffic reaches the API server until an operator explicitly adds a range here. Must not include 0.0.0.0/0. CONSEQUENCE: a standard GitHub-hosted runner (ubuntu-latest) has no stable CIDR to allowlist, so var.deploy_kubernetes_resources=true from ordinary CI needs a self-hosted/VNet-reachable runner rather than an entry here."
  type = list(object({
    cidr_block   = string
    display_name = optional(string, "")
  }))
  default = []

  validation {
    condition     = !contains([for n in var.master_authorized_networks : n.cidr_block], "0.0.0.0/0")
    error_message = "master_authorized_networks must not contain 0.0.0.0/0; that reopens the control plane's public endpoint to the whole internet, exactly what this allowlist exists to prevent."
  }
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
