# OIDC signing key retention (#324). destroy_scheduled_duration is a
# create-time-only (ForceNew) attribute, so the default must stay the value
# every deployed key was created with, and an out-of-range value must fail at
# plan time rather than at the Cloud KMS API. Every provider is mocked; nothing
# here reaches GCP. create_before_destroy is asserted on the source by
# TestGCPSigningKeyCreatesBeforeDestroy, since terraform test cannot see
# lifecycle settings.
mock_provider "google" {}

variables {
  project_id                  = "cudly-test-project"
  service_name                = "cudly-test"
  environment                 = "dev"
  region                      = "us-central1"
  image_uri                   = "us-central1-docker.pkg.dev/cudly-test-project/cudly/cudly:test"
  database_host               = "10.0.0.5"
  database_name               = "cudly"
  database_username           = "cudly"
  database_password_secret_id = "cudly-test-db-password"
  admin_email                 = "admin@example.com"
}

run "default_keeps_deployed_keys_unchanged" {
  command = plan

  plan_options {
    target = [google_kms_crypto_key.signing]
  }

  assert {
    condition     = google_kms_crypto_key.signing.destroy_scheduled_duration == "86400s"
    error_message = "The default must stay 86400s: every deployed key was created with it, and a different value forces the key's replacement."
  }
}

run "accepts_longer_window_for_new_keys" {
  command = plan

  plan_options {
    target = [google_kms_crypto_key.signing]
  }

  variables {
    signing_key_destroy_scheduled_duration = "2592000s"
  }

  assert {
    condition     = google_kms_crypto_key.signing.destroy_scheduled_duration == "2592000s"
    error_message = "The input must reach the crypto key."
  }
}

run "accepts_upper_bound" {
  command = plan

  plan_options {
    target = [google_kms_crypto_key.signing]
  }

  variables {
    signing_key_destroy_scheduled_duration = "10368000s"
  }
}

run "rejects_below_one_day" {
  command = plan

  plan_options {
    target = [google_kms_crypto_key.signing]
  }

  variables {
    signing_key_destroy_scheduled_duration = "86399s"
  }

  expect_failures = [var.signing_key_destroy_scheduled_duration]
}

run "rejects_above_120_days" {
  command = plan

  plan_options {
    target = [google_kms_crypto_key.signing]
  }

  variables {
    signing_key_destroy_scheduled_duration = "10368001s"
  }

  expect_failures = [var.signing_key_destroy_scheduled_duration]
}

run "rejects_non_seconds_format" {
  command = plan

  plan_options {
    target = [google_kms_crypto_key.signing]
  }

  variables {
    signing_key_destroy_scheduled_duration = "30d"
  }

  expect_failures = [var.signing_key_destroy_scheduled_duration]
}
