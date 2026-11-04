variable "subscription_id" {
  description = "Azure subscription ID"
  type        = string
}

variable "github_repo" {
  description = "GitHub repository (owner/name) whose Actions workflows may authenticate via federated identity credentials (e.g. 'LeanerCloud/cloud-commitments-platform'). Leave empty to skip federated credential setup."
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

variable "github_environments" {
  description = <<-EOT
    GitHub deployment environment names whose jobs may authenticate via federated
    identity credentials. A job carrying `environment: <name>` presents the OIDC
    subject repo:<owner>@<owner_id>/<repo>@<repo_id>:environment:<name> (immutable ids), so a name absent from this list
    cannot authenticate (AADSTS70021).

    NOTE the subject is ref-agnostic: it does not encode the branch, so a
    credential here lets ANY branch that can reach a job bound to that environment
    obtain the deploy service principal. Add a name only once a workflow actually
    presents it, and restrict the branch via the environment's
    deployment_branch_policy — an in-workflow ref check does not bind, because
    workflow_dispatch runs the file as it exists on the dispatched ref.

    Gating rule: add a name only once its GitHub environment exists with a
    deployment branch policy that admits only `main`. This service principal
    already trusts the same immutable-id main-branch subject (sp.tf, github_main),
    and only a run on `main` can enter such an environment, so its credential
    adds no reach. A credential for an environment that does not exist widens
    access: GitHub creates a referenced environment on first use with no
    protection, so any branch could bind it and obtain this service principal.
    Environments are repository settings, not managed here; check with
    `gh api repos/<github_repo>/environments/<name>/deployment-branch-policies`.

    The default meets the rule, `prod` included: `dev`, `staging` and `prod`
    each exist with a `main`-only branch policy (applied 2026-10-04, see #141
    and #2), and `prod` uses the same service principal as the other two, so it
    is not an exception. None has required reviewers, so the branch policy
    (plus main's ruleset) is the only gate. Widening a policy past `main`, for
    example a `v*` tag policy for release-triggered `prod` deploys, widens this
    credential's reach to those refs and needs the same review.

    This list is NOT a complete enumeration of the environment subjects this repo
    presents to Azure. It covers the destroy jobs bound by cleanup-staging.yml
    (`staging`) and destroy-fargate-dev.yml (`dev`), deploy-azure.yml's
    build-and-deploy and test-deployment jobs (see #140), and rollback.yml's
    rollback-azure job (see #139) — all of which bind plain
    `dev`/`staging`/`prod`.

    Knowingly NOT covered, tracked in #1648 — this Azure job binds a compound
    environment name and therefore still fails with AADSTS70021:
      - database-migration.yml -> azure-db-{dev,staging,prod}

    They were excluded because their environments did not exist yet. Those
    environments now exist with a `main`-only branch policy (2026-10-04), so
    they meet the gating rule; adding them is left to #1648 because it mints
    three new credentials and needs a bootstrap re-apply. Do not read the
    absence of a name as "no job uses it".
  EOT
  type        = list(string)
  default     = ["dev", "staging", "prod"]

  validation {
    condition     = length(var.github_environments) == length(distinct(var.github_environments))
    error_message = "github_environments must not contain duplicates; toset() would silently collapse them, so a duplicate indicates a typo."
  }

  validation {
    condition     = alltrue([for e in var.github_environments : trimspace(e) != ""])
    error_message = "github_environments entries must be non-empty; an empty name yields the unmatchable subject repo:<repo>:environment:."
  }
}
