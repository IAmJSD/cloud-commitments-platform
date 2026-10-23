# Azure AKS Module Variables

variable "project_name" {
  description = "Project name for resource naming"
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
  description = "Azure location"
  type        = string
}

variable "vnet_subnet_id" {
  description = "Subnet ID for AKS nodes"
  type        = string
}

variable "image_name" {
  description = "Container image name (without registry)"
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
  default     = "1.28"
}

variable "node_count" {
  description = "Number of nodes in the default node pool"
  type        = number
  default     = 2
}

variable "node_vm_size" {
  description = "VM size for nodes"
  type        = string
  default     = "Standard_D2s_v3"
}

variable "min_node_count" {
  description = "Minimum node count for auto-scaling"
  type        = number
  default     = 1
}

variable "max_node_count" {
  description = "Maximum node count for auto-scaling"
  type        = number
  default     = 10
}

variable "database_host" {
  description = "PostgreSQL server FQDN"
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
  description = "Name of Key Vault secret containing database password"
  type        = string
}

variable "key_vault_id" {
  description = "Key Vault ID for secrets"
  type        = string
}

variable "enable_auto_scaling" {
  description = "Enable cluster auto-scaling"
  type        = bool
  default     = true
}

variable "enable_azure_policy" {
  description = "Enable Azure Policy add-on"
  type        = bool
  default     = false
}

variable "enable_log_analytics" {
  description = "Enable Log Analytics"
  type        = bool
  default     = true
}

variable "deploy_kubernetes_resources" {
  description = "Deploy kubernetes resources (namespace, deployment, service, etc). Requires kubernetes/helm providers to be configured at root level. Set to false to only create the AKS cluster."
  type        = bool
  default     = false
}

variable "admin_email" {
  description = "Administrator email address"
  type        = string
  default     = ""
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

variable "key_vault_uri" {
  description = "Key Vault URI for secrets"
  type        = string
  default     = ""
}

variable "service_cidr" {
  description = "Kubernetes service CIDR"
  type        = string
  default     = "10.0.0.0/16"
}

variable "dns_service_ip" {
  description = "Kubernetes DNS service IP (must be within service_cidr)"
  type        = string
  default     = "10.0.0.10"
}

variable "nginx_ingress_version" {
  description = "NGINX Ingress Controller Helm chart version"
  type        = string
  default     = "4.8.0"
}

variable "tags" {
  description = "Additional tags for resources"
  type        = map(string)
  default     = {}
}

variable "private_cluster_enabled" {
  description = "Disable the AKS API server's public endpoint. Defaults to true (private cluster). Set to false only alongside a real, non-empty var.authorized_ip_ranges allowlist -- an operator who disables this without one gets a plan-time error, not a public-by-accident cluster. CONSEQUENCE: with this true (the default), the API server is reachable only from inside the VNet, so terraform apply itself cannot reach it from a standard GitHub-hosted runner (ubuntu-latest) unless that runner has VNet connectivity (self-hosted runner, VPN, or a peered network) -- and the kubernetes/helm providers this module's deploy_kubernetes_resources=true path configures (see environments/azure/main.tf) will time out trying to reach a private endpoint from a non-VNet runner. Only set var.deploy_kubernetes_resources=true from a VNet-reachable apply context."
  type        = bool
  default     = true

  validation {
    condition     = var.private_cluster_enabled || length(var.authorized_ip_ranges) > 0
    error_message = "private_cluster_enabled=false requires a non-empty authorized_ip_ranges allowlist; otherwise the AKS API server is reachable from 0.0.0.0/0."
  }
}

variable "authorized_ip_ranges" {
  description = "CIDR allowlist for the public API server when private_cluster_enabled=false (e.g. CI egress ranges plus an operator bastion). Ignored when private_cluster_enabled=true. Must not include 0.0.0.0/0."
  type        = list(string)
  default     = []

  validation {
    condition     = !contains(var.authorized_ip_ranges, "0.0.0.0/0")
    error_message = "authorized_ip_ranges must not contain 0.0.0.0/0; that reopens the public endpoint to the whole internet, exactly what private_cluster_enabled/authorized_ip_ranges exists to prevent."
  }
}

variable "local_account_disabled" {
  description = "Disable AKS's static cluster-admin client certificate (obtained via `az aks get-credentials --admin`), which never expires and isn't tied to any Entra identity. Defaults to true; admin access is instead granted via Azure RBAC role assignments (e.g. \"Azure Kubernetes Service RBAC Cluster Admin\") or var.admin_group_object_ids. CONSEQUENCE: with this true (the default), the Kubernetes provider's `client_certificate`/`client_key` auth (as environments/azure/main.tf currently configures it) stops working, because AKS no longer issues that certificate -- terraform's kubernetes/helm providers need `exec`-based auth (e.g. kubelogin, or an Azure RBAC-authorized service principal token) instead. Do not set var.deploy_kubernetes_resources=true with the default (true) until the provider block is switched to exec auth."
  type        = bool
  default     = true
}

variable "admin_group_object_ids" {
  description = "Entra ID (Azure AD) group object IDs granted AKS admin access via azure_active_directory_role_based_access_control. Optional: with azure_rbac_enabled=true (always set by this module), admin access can also be granted purely through Azure RBAC role assignments against the cluster resource, so an empty list is a valid, non-locking-out default."
  type        = list(string)
  default     = []
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
