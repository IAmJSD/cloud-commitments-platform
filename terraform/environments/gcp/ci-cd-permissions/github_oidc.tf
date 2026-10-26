# Principal identifiers are pool-scoped, not provider-scoped: any provider added
# to this pool that can mint the same attribute value satisfies the
# impersonation grant below. Keep this pool dedicated to CUDly's deploy trust;
# do not add a second provider to it. (Renaming the pool to a CUDly-scoped id
# replaces it and the provider, which changes GCP_WORKLOAD_IDENTITY_PROVIDER for
# every GCP workflow, so it is left to a planned bootstrap migration.)
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

    # Repo id and ref in one attribute, so the impersonation member below can
    # pin both: a principalSet names a single attribute. repository_id is all
    # digits, so the first ':' separates the two unambiguously.
    "attribute.repository_id_ref" = "assertion.repository_id + ':' + assertion.ref"
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
#
# The member pins the ref as well as the repo id, so the grant itself names the
# same repo+ref the provider's attribute_condition admits. Keyed on the repo id
# alone, any other provider in the pool that maps attribute.repository_id for
# this repo would satisfy the grant from any branch, PR or tag.
resource "google_service_account_iam_member" "github_actions" {
  count = var.github_repo != "" ? 1 : 0

  service_account_id = google_service_account.cudly_deploy.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/${google_iam_workload_identity_pool.github[0].name}/attribute.repository_id_ref/${var.github_repo_id}:${var.deploy_ref}"

  # Upgrade ordering. member is ForceNew, so changing it replaces this binding:
  #   depends_on:            the provider gains the repository_id_ref mapping
  #                          (an in-place update) before the new member exists.
  #   create_before_destroy: the new member is added while the old one (keyed
  #                          on attribute.repository_id, whose mapping stays) is
  #                          still present.
  # Every step keeps at least one matching grant, so no access gap opens.
  depends_on = [google_iam_workload_identity_pool_provider.github]

  lifecycle {
    create_before_destroy = true
  }
}
