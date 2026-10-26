# The migrations need PostgreSQL 15+ (000134 uses UNIQUE NULLS NOT DISTINCT),
# so an older engine must fail at plan time, not at migration time (#1233).
#
# Every provider is mocked, so nothing here reaches GCP. Each run targets
# module.database, the input's only consumer: the targeted graph still
# evaluates the variable's validation, without planning the whole
# environment against mock data.
mock_provider "google" {}
mock_provider "google-beta" {}
mock_provider "kubernetes" {}
mock_provider "helm" {}
mock_provider "http" {}
mock_provider "external" {}
mock_provider "random" {}

variables {
  project_id  = "cudly-test-project"
  admin_email = "admin@example.com"

  # A fixed placeholder key: the auto-generated one is unknown at plan time,
  # and the secrets module counts on it.
  credential_encryption_key = "0000000000000000000000000000000000000000000000000000000000000000"
}

run "rejects_postgres_14" {
  command = plan

  plan_options {
    target = [module.database]
  }

  variables {
    database_version = "POSTGRES_14"
  }

  expect_failures = [var.database_version]
}

run "accepts_postgres_15" {
  command = plan

  plan_options {
    target = [module.database]
  }

  variables {
    database_version = "POSTGRES_15"
  }
}

run "accepts_postgres_16" {
  command = plan

  plan_options {
    target = [module.database]
  }

  variables {
    database_version = "POSTGRES_16"
  }
}
