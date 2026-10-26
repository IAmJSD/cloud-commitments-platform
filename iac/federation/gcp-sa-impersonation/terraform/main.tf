terraform {
  required_version = ">= 1.5"
  required_providers {
    google = {
      # The ceiling admits google 8.x although our lock resolves 7.x: this
      # bundle ships without its lock file, so customers who onboarded after
      # google 8.0 came out already run 8.x, and a < 8.0 ceiling would force
      # them to downgrade. It still stops an unreviewed 9.x (#12).
      source  = "hashicorp/google"
      version = ">= 5.0, < 9.0"
    }
    http = {
      source  = "hashicorp/http"
      version = ">= 3.4, < 4.0"
    }
  }
}

# Grant the source SA permission to impersonate the target SA.
# Use _member (not _binding) to avoid replacing other existing bindings.
resource "google_service_account_iam_member" "cudly_impersonate" {
  service_account_id = "projects/${var.project_id}/serviceAccounts/${var.service_account_email}"
  role               = "roles/iam.serviceAccountTokenCreator"
  member             = "serviceAccount:${var.source_service_account}"
}

# Grant the target SA permissions needed to manage CUDs/commitments.
#
# roles/commerceorgpolicy.commitmentAdmin and roles/billing.viewer are
# organization- and billing-account-scoped respectively; granting either at
# project scope via google_project_iam_member 400s (same failure mode
# terraform/modules/compute/gcp/cloud-run/main.tf:484 documents for
# billing.viewer, and federation/gcp-target/terraform/variables.tf:198-201
# documents for commitmentAdmin). Neither can work here.
#
# Instead grant exactly what CUDly needs, mirroring the sibling
# gcp-target bundle (iac/federation/gcp-target/terraform/main.tf) so the
# two onboarding paths converge on the same permission set: a custom role
# holding only the two commitment-write permissions, plus the built-in
# compute.viewer role for read access (regions/zones/machineTypes/commitments
# .list/.get). There is no billing-read path in this bundle (no
# billing_account_id input to scope roles/billing.viewer to), so it is
# dropped rather than reintroduced broken.
#
# var.create_custom_role gates creation: if the same project was already
# onboarded through federation/gcp-target (or self-hosts CUDly via
# terraform/modules/compute/gcp/cloud-run), that call site already created a
# role with the same var.custom_role_id, and creating it again here 409s
# (custom role IDs are unique per project, and each bundle applies from its
# own Terraform state, so there's no shared state to reconcile against).
# Default true preserves this bundle's own behavior for the common case
# where it's the only onboarding path applied to the project.
resource "google_project_iam_custom_role" "cudly" {
  count   = var.create_custom_role ? 1 : 0
  project = var.project_id
  role_id = var.custom_role_id
  title   = "CUDly Commitment Writer"
  # Description matches terraform/modules/compute/gcp/cloud-run and
  # federation/gcp-target exactly so applies from any of the three don't
  # stomp each other.
  description = "Minimum permissions for CUDly to purchase and update committed use discounts."
  permissions = var.custom_role_permissions
  stage       = "GA"
}

locals {
  # Reference the role this bundle created, or (when var.create_custom_role
  # is false) the identically-shaped role another call site already created
  # in this project. google_project_iam_custom_role.id renders to exactly
  # this format.
  custom_role_id = var.create_custom_role ? google_project_iam_custom_role.cudly[0].id : "projects/${var.project_id}/roles/${var.custom_role_id}"
}

resource "google_project_iam_member" "cudly_custom" {
  project = var.project_id
  role    = local.custom_role_id
  member  = "serviceAccount:${var.service_account_email}"
}

# Built-in read-side roles, mirroring federation/gcp-target's
# service_account_project_roles: roles/compute.viewer (regions/zones/
# machineTypes/commitments.list/.get) and roles/recommender.viewer. The
# latter is required by the collection pipeline
# (providers/gcp/services/computeengine.GetRecommendations calls the GCP
# Recommender API, "google.billing.CostInsight.commitmentRecommender");
# without it, every recommendation call 403s and is caught and warn-logged
# by providers/gcp/recommendations.go, so an onboarded account would
# silently surface zero GCP recommendations rather than erroring.
resource "google_project_iam_member" "cudly_built_in" {
  for_each = toset(var.built_in_project_roles)
  project  = var.project_id
  role     = each.value
  member   = "serviceAccount:${var.service_account_email}"
}
