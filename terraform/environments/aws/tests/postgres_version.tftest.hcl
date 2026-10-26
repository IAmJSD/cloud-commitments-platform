# The migrations need PostgreSQL 15+ (000134 uses UNIQUE NULLS NOT DISTINCT),
# so an older engine must fail at plan time, not at migration time (#1233).
#
# Every provider is mocked, so nothing here reaches AWS. Each run targets
# module.database, the input's only consumer: the targeted graph still
# evaluates the variable's validation, without planning the whole
# environment against mock data.
mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = { account_id = "123456789012" }
  }

  # The networking module indexes one subnet per availability zone.
  mock_data "aws_availability_zones" {
    defaults = { names = ["us-east-1a", "us-east-1b", "us-east-1c"] }
  }
}

mock_provider "random" {}
mock_provider "http" {}
mock_provider "external" {}

variables {
  admin_email = "admin@example.com"
}

run "rejects_postgres_14" {
  command = plan

  plan_options {
    target = [module.database]
  }

  variables {
    database_engine_version = "14"
  }

  expect_failures = [var.database_engine_version]
}

run "rejects_postgres_14_minor" {
  command = plan

  plan_options {
    target = [module.database]
  }

  variables {
    database_engine_version = "14.12"
  }

  expect_failures = [var.database_engine_version]
}

run "rejects_malformed_version" {
  command = plan

  plan_options {
    target = [module.database]
  }

  variables {
    database_engine_version = "sixteen"
  }

  expect_failures = [var.database_engine_version]
}

run "accepts_postgres_15" {
  command = plan

  plan_options {
    target = [module.database]
  }

  variables {
    database_engine_version = "15"
  }
}

run "accepts_postgres_16" {
  command = plan

  plan_options {
    target = [module.database]
  }

  variables {
    database_engine_version = "16"
  }
}

run "accepts_postgres_16_minor" {
  command = plan

  plan_options {
    target = [module.database]
  }

  variables {
    database_engine_version = "16.6"
  }
}
