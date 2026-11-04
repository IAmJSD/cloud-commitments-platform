# =============================================================================
# Azure AD Service Principal for CI/CD
# =============================================================================
#
# If you already have an existing SP and want to adopt it instead of creating
# a new one, import both the application and the SP before applying:
#
#   # Get app object ID (different from the client/app ID):
#   az ad app show --id <appId> --query id -o tsv
#
#   terraform import azuread_application.cudly_deploy <app-object-id>
#   terraform import azuread_service_principal.cudly_deploy <sp-object-id>

resource "azuread_application" "cudly_deploy" {
  display_name = "cudly-terraform-deploy"
}

resource "azuread_service_principal" "cudly_deploy" {
  client_id = azuread_application.cudly_deploy.client_id
}

# =============================================================================
# GitHub Actions Federated Identity Credentials
# =============================================================================
# Azure federated credentials require one entry per allowed subject (no
# wildcards), so each named deployment environment needs its own resource.
#
# The `github_environment` entries below are what let an environment-bound job
# authenticate at all. A job carrying `environment: staging` presents the
# subject `repo:<owner>@<id>/<repo>@<id>:environment:staging`, NOT the main-branch subject,
# so without a matching credential `azure/login` fails with AADSTS70021 even
# though the workflow is running on main. Adding an `environment:` binding to
# an Azure job and forgetting this is the exact breakage recorded in #1648.
#
# NOTE: this module is bootstrap-only (see CLAUDE.md) — it is applied manually
# by a privileged human, not by the deploy workflow. The Azure destroy job in
# cleanup-staging.yml cannot authenticate until that re-apply happens.

# GitHub's immutable OIDC subject prefix (repo setting use_immutable_subject):
# repo:<owner>@<owner_id>/<name>@<repo_id>. A renamed or delete-and-recreated
# repo gets a new id, so a name squatter cannot match. Never reintroduce the
# name-only form.
locals {
  github_subject_prefix = var.github_repo == "" ? "" : "repo:${split("/", var.github_repo)[0]}@${var.github_owner_id}/${split("/", var.github_repo)[1]}@${var.github_repo_id}"
}

resource "azuread_application_federated_identity_credential" "github_main" {
  count = var.github_repo != "" ? 1 : 0

  application_id = azuread_application.cudly_deploy.id
  display_name   = "github-actions-main"
  description    = "GitHub Actions OIDC — ${var.github_repo} main branch deployments"
  audiences      = ["api://AzureADTokenExchange"]
  issuer         = "https://token.actions.githubusercontent.com"
  subject        = "${local.github_subject_prefix}:ref:refs/heads/main"
}

# No `pull_request`-subject credential: this service principal holds
# `CUDly Terraform Deploy` (Key Vault secrets data-plane, state-blob
# read/write, managed identity and Container App data-plane — see role.tf /
# locals_compute.tf). A `pull_request` subject is not scoped to this repo's
# branch or environment protection at all, so any PR that touches a workflow
# file (or reaches an existing `pull_request`-triggered job requesting
# `id-token: write`) would obtain it. No workflow in this repo runs
# `azure/login` on a `pull_request` trigger — the read-only PR sanity checks
# that used to need this credential moved to cloud-commitments-go with the
# repo split — so this credential has no consumer. See #90.
#
# If a read-only PR plan check is ever added here, point it at a separate
# service principal scoped to Reader-only roles with no Key Vault data-plane
# and no state-write permissions, rather than re-adding this credential.

# One credential per named deployment environment, so environment-bound jobs
# can authenticate. Mirrors the `environment:{dev,staging,prod}` subjects the
# AWS role's trust policy already allows
# (terraform/environments/aws/ci-cd-permissions/role.tf).
resource "azuread_application_federated_identity_credential" "github_environment" {
  for_each = var.github_repo != "" ? toset(var.github_environments) : toset([])

  application_id = azuread_application.cudly_deploy.id
  display_name   = "github-actions-env-${each.value}"
  description    = "GitHub Actions OIDC — ${var.github_repo} ${each.value} environment"
  audiences      = ["api://AzureADTokenExchange"]
  issuer         = "https://token.actions.githubusercontent.com"
  subject        = "${local.github_subject_prefix}:environment:${each.value}"
}
