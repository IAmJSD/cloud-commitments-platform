variable "aws_region" {
  description = "AWS region for the provider and state backend"
  type        = string
  default     = "us-east-1"
}

variable "trust_principal" {
  description = "IAM principal allowed to assume the deploy role for local deployments, relative to the account ARN (e.g. 'user/alice', 'role/AdminRole'). Leave empty if only GitHub Actions OIDC is needed."
  type        = string
  default     = ""
}

variable "github_repo" {
  description = "GitHub repository (owner/name) whose Actions workflows may assume the deploy role via OIDC (e.g. 'LeanerCloud/cloud-commitments-platform'). Leave empty to skip OIDC setup."
  type        = string
  default     = "LeanerCloud/cloud-commitments-platform"

  validation {
    condition     = var.github_repo == "" || can(regex("^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$", var.github_repo))
    error_message = "github_repo must be empty or owner/name."
  }
}

variable "github_owner_id" {
  description = "Immutable numeric id of the GitHub owner (gh api repos/OWNER/REPO --jq .owner.id). Trust subjects are keyed on it, not on the owner name."
  type        = string

  validation {
    condition     = can(regex("^[1-9][0-9]*$", var.github_owner_id))
    error_message = "github_owner_id must be a positive integer without leading zeros."
  }
}

variable "github_repo_id" {
  description = "Immutable numeric id of the GitHub repository (gh api repos/OWNER/REPO --jq .id). Trust subjects are keyed on it, not on the repo name."
  type        = string

  validation {
    condition     = can(regex("^[1-9][0-9]*$", var.github_repo_id))
    error_message = "github_repo_id must be a positive integer without leading zeros."
  }
}
