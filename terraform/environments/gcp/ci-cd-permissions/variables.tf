variable "project_id" {
  description = "GCP project ID"
  type        = string
}

variable "github_repo" {
  description = "GitHub repository (owner/name) whose Actions workflows may impersonate the deploy SA via Workload Identity Federation (e.g. 'LeanerCloud/cloud-commitments-platform'). Leave empty to skip WIF setup."
  type        = string
  default     = "LeanerCloud/cloud-commitments-platform"

  validation {
    condition     = var.github_repo == "" || can(regex("^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$", var.github_repo))
    error_message = "github_repo must be empty or owner/name."
  }
}

variable "github_owner_id" {
  description = "Immutable numeric id of the GitHub owner (gh api repos/OWNER/REPO --jq .owner.id). Trust is keyed on it, not on the owner name."
  type        = string

  validation {
    condition     = can(regex("^[1-9][0-9]*$", var.github_owner_id))
    error_message = "github_owner_id must be a positive integer without leading zeros."
  }
}

variable "github_repo_id" {
  description = "Immutable numeric id of the GitHub repository (gh api repos/OWNER/REPO --jq .id). Trust is keyed on it, not on the repo name."
  type        = string

  validation {
    condition     = can(regex("^[1-9][0-9]*$", var.github_repo_id))
    error_message = "github_repo_id must be a positive integer without leading zeros."
  }
}

variable "deploy_ref" {
  description = "Git ref allowed to deploy (e.g. refs/heads/main). Only workflows from this exact ref can impersonate the deploy SA. PRs, tags, and other branches are blocked."
  type        = string
  default     = "refs/heads/main"

  validation {
    condition     = can(regex("^refs/(heads|tags)/[A-Za-z0-9._/-]+$", var.deploy_ref))
    error_message = "deploy_ref must look like refs/heads/<branch> or refs/tags/<tag> (it is interpolated into the attribute condition)."
  }
}
