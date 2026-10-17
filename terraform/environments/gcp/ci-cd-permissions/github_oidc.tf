resource "google_iam_workload_identity_pool" "github" {
  count = var.github_repo != "" ? 1 : 0

  workload_identity_pool_id = "github-actions"
  display_name              = "GitHub Actions"
  description               = "Workload Identity Pool for GitHub Actions OIDC"
  project                   = var.project_id
}

resource "google_iam_workload_identity_pool_provider" "github" {
  count = var.github_repo != "" ? 1 : 0

  workload_identity_pool_id          = google_iam_workload_identity_pool.github[0].workload_identity_pool_id
  workload_identity_pool_provider_id = "github-actions"
  display_name                       = "GitHub Actions"
  project                            = var.project_id

  oidc {
    issuer_uri = "https://token.actions.githubusercontent.com"
  }

  # Map GitHub token claims to Google attributes for use in conditions and bindings.
  attribute_mapping = {
    "google.subject"       = "assertion.sub"
    "attribute.actor"      = "assertion.actor"
    "attribute.repository" = "assertion.repository"
    "attribute.ref"        = "assertion.ref"

    # Immutable ids: a renamed or deleted-and-recreated repo gets a new id, so
    # a name squatter cannot match.
    "attribute.repository_id"       = "assertion.repository_id"
    "attribute.repository_owner_id" = "assertion.repository_owner_id"
  }

  # Restrict to the specific repo (by immutable ids, not name) AND only allow the designated deploy branch.
  # PRs, tags, forks, and workflow_dispatch from other refs are blocked.
  #
  # deploy_ref defaults to refs/heads/main. Override in terraform.tfvars only
  # when temporarily deploying from a feature branch; flip back to main once
  # that branch merges. Forgetting to flip locks out main-branch deploys with
  # "unauthorized_client: The given credential is rejected by the attribute condition."
  attribute_condition = "assertion.repository_owner_id == '${var.github_owner_id}' && assertion.repository_id == '${var.github_repo_id}' && assertion.ref == '${var.deploy_ref}'"
}

# Allow workflows from the permitted repo+ref to impersonate the deploy SA.
resource "google_service_account_iam_member" "github_actions" {
  count = var.github_repo != "" ? 1 : 0

  service_account_id = google_service_account.cudly_deploy.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/${google_iam_workload_identity_pool.github[0].name}/attribute.repository_id/${var.github_repo_id}"

  # The old member (keyed on attribute.repository) keeps resolving because that
  # mapping stays, so the new binding is created first and no access gap opens.
  lifecycle {
    create_before_destroy = true
  }
}
