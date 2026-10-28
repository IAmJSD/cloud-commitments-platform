# GCP Cloud KMS asymmetric signing key used by the CUDly OIDC issuer
# to sign client-assertion JWTs presented to target-cloud token
# endpoints. The private half never leaves Cloud KMS. The public half
# is published through Cloud Run's /.well-known/jwks.json endpoint.

# Enable the Cloud KMS API before creating the key ring — fresh
# projects and projects that have never used KMS before will 403
# Error creating KeyRing: Cloud Key Management Service (KMS) API
# has not been used in project <number> before or it is disabled.
resource "google_project_service" "cloudkms" {
  project            = var.project_id
  service            = "cloudkms.googleapis.com"
  disable_on_destroy = false
}

resource "google_kms_key_ring" "signing" {
  name     = "cudly-oidc-signing"
  location = var.region
  project  = var.project_id

  depends_on = [google_project_service.cloudkms]
}

resource "google_kms_crypto_key" "signing" {
  name                       = "cudly-oidc-signing"
  key_ring                   = google_kms_key_ring.signing.id
  purpose                    = "ASYMMETRIC_SIGN"
  destroy_scheduled_duration = var.signing_key_destroy_scheduled_duration

  version_template {
    algorithm        = "EC_SIGN_P256_SHA256"
    protection_level = "SOFTWARE"
  }

  # Matches the AWS twin (modules/compute/aws/lambda/signing-key.tf, cli#1480):
  # a change that forces a new key (purpose, the algorithm, the duration
  # above, or the key ring's location via var.region) must not destroy the
  # live signing key first. Destroying a crypto key schedules every version
  # for destruction, which stops AsymmetricSign at once, so the default
  # destroy-then-create order breaks client-assertion JWT minting for every
  # federated target cloud. Create-first either builds the new key ring and
  # key before the old key goes (a region move), or, because Cloud KMS never
  # frees a key name, fails on the name collision before the old key is
  # touched (any in-place replacement). Either way the live key keeps signing
  # until there is a working replacement (#324). Terraform also applies
  # create_before_destroy to google_kms_key_ring.signing, which this key
  # depends on, so a region move creates the new ring first as well. That is
  # the intended order.
  #
  # There is deliberately no prevent_destroy. Terraform only accepts a literal
  # there, so it cannot vary per environment, and cleanup-staging.yml runs
  # `terraform destroy` on staging, which prevent_destroy = true would fail.
  lifecycle {
    create_before_destroy = true
  }
}

# The Cloud Run service account needs cloudkms.signer to call
# AsymmetricSign and cloudkms.viewer to call GetPublicKey.
resource "google_kms_crypto_key_iam_member" "cloud_run_signer" {
  crypto_key_id = google_kms_crypto_key.signing.id
  role          = "roles/cloudkms.signer"
  member        = "serviceAccount:${google_service_account.cloud_run.email}"
}

resource "google_kms_crypto_key_iam_member" "cloud_run_viewer" {
  crypto_key_id = google_kms_crypto_key.signing.id
  role          = "roles/cloudkms.publicKeyViewer"
  member        = "serviceAccount:${google_service_account.cloud_run.email}"
}

# The signing key version resource name CUDly passes to internal/oidc
# as CUDLY_SIGNING_KEY_RESOURCE. Cloud KMS creates version "1"
# automatically when a crypto key is first created.
locals {
  signing_key_version_resource = "${google_kms_crypto_key.signing.id}/cryptoKeyVersions/1"
}
