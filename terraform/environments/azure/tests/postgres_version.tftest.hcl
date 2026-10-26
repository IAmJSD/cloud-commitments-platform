# The migrations need PostgreSQL 15+ (000134 uses UNIQUE NULLS NOT DISTINCT),
# so an older engine must fail at plan time, not at migration time (#1233).
#
# Every provider is mocked, so nothing here reaches Azure. Each run targets
# module.database, the input's only consumer: the targeted graph still
# evaluates the variable's validation, without planning the whole
# environment against mock data.
mock_provider "azurerm" {}
mock_provider "azuread" {}
mock_provider "random" {}
mock_provider "kubernetes" {}
mock_provider "helm" {}
mock_provider "http" {}
mock_provider "null" {}
mock_provider "external" {}

variables {
  subscription_id = "00000000-0000-0000-0000-000000000000"
  admin_email     = "admin@example.com"
}

run "rejects_postgres_14" {
  command = plan

  plan_options {
    target = [module.database]
  }

  variables {
    postgres_version = "14"
  }

  expect_failures = [var.postgres_version]
}

run "accepts_postgres_15" {
  command = plan

  plan_options {
    target = [module.database]
  }

  variables {
    postgres_version = "15"
  }
}

run "accepts_postgres_16" {
  command = plan

  plan_options {
    target = [module.database]
  }

  variables {
    postgres_version = "16"
  }
}
