# Lambda secrets policy must fail closed on the database secret ARN (#291).
# The GetSecretValue resource list adds "${arn}*" for each secret; the
# database entry was unguarded, so an empty ARN rendered the bare "*" (which
# compact() keeps) and granted read on every secret in the account. The input
# is now validated, and the entry is guarded like its optional siblings.
mock_provider "aws" {}

variables {
  stack_name                   = "cudly-test"
  permissions_boundary_arn     = "arn:aws:iam::123456789012:policy/cudly-boundary"
  environment                  = "dev"
  region                       = "us-east-1"
  image_uri                    = "123456789012.dkr.ecr.us-east-1.amazonaws.com/cudly:test"
  database_host                = "db.example.com"
  database_name                = "cudly"
  database_username            = "cudly"
  database_password_secret_arn = "arn:aws:secretsmanager:us-east-1:123456789012:secret:cudly-dev-db-AbCdEf"
  admin_email                  = "admin@example.com"
  allowed_origins              = ["https://app.example.com"]
}

run "db_secret_only_grants_that_secret" {
  command = plan

  assert {
    condition = jsondecode(aws_iam_role_policy.secrets_access.policy).Statement[0].Resource == [
      "arn:aws:secretsmanager:us-east-1:123456789012:secret:cudly-dev-db-AbCdEf",
      "arn:aws:secretsmanager:us-east-1:123456789012:secret:cudly-dev-db-AbCdEf*",
    ]
    error_message = "With only the database secret set, GetSecretValue must cover exactly that ARN and its suffix pattern."
  }

  assert {
    condition = alltrue([
      for s in jsondecode(aws_iam_role_policy.secrets_access.policy).Statement :
      !contains(flatten([s.Resource]), "*")
    ])
    error_message = "No secrets policy statement may grant Resource \"*\"."
  }
}

run "optional_secrets_add_their_own_patterns_only" {
  command = plan

  variables {
    admin_password_secret_arn            = "arn:aws:secretsmanager:us-east-1:123456789012:secret:cudly-dev-admin-AbCdEf"
    credential_encryption_key_secret_arn = "arn:aws:secretsmanager:us-east-1:123456789012:secret:cudly-dev-credkey-AbCdEf"
  }

  assert {
    condition = jsondecode(aws_iam_role_policy.secrets_access.policy).Statement[0].Resource == [
      "arn:aws:secretsmanager:us-east-1:123456789012:secret:cudly-dev-db-AbCdEf",
      "arn:aws:secretsmanager:us-east-1:123456789012:secret:cudly-dev-db-AbCdEf*",
      "arn:aws:secretsmanager:us-east-1:123456789012:secret:cudly-dev-admin-AbCdEf",
      "arn:aws:secretsmanager:us-east-1:123456789012:secret:cudly-dev-admin-AbCdEf*",
      "arn:aws:secretsmanager:us-east-1:123456789012:secret:cudly-dev-credkey-AbCdEf",
      "arn:aws:secretsmanager:us-east-1:123456789012:secret:cudly-dev-credkey-AbCdEf*",
    ]
    error_message = "Each configured secret must contribute exactly its ARN and suffix pattern."
  }
}

run "accepts_govcloud_partition" {
  command = plan

  variables {
    database_password_secret_arn = "arn:aws-us-gov:secretsmanager:us-gov-west-1:123456789012:secret:cudly/db_pass-AbCdEf"
  }

  assert {
    condition     = contains(jsondecode(aws_iam_role_policy.secrets_access.policy).Statement[0].Resource, "arn:aws-us-gov:secretsmanager:us-gov-west-1:123456789012:secret:cudly/db_pass-AbCdEf*")
    error_message = "A GovCloud secret ARN must be accepted and granted."
  }
}

run "rejects_empty_db_secret_arn" {
  command = plan

  variables {
    database_password_secret_arn = ""
  }

  expect_failures = [var.database_password_secret_arn]
}

run "rejects_bare_wildcard" {
  command = plan

  variables {
    database_password_secret_arn = "*"
  }

  expect_failures = [var.database_password_secret_arn]
}

run "rejects_wildcard_secret_name" {
  command = plan

  variables {
    database_password_secret_arn = "arn:aws:secretsmanager:us-east-1:123456789012:secret:*"
  }

  expect_failures = [var.database_password_secret_arn]
}

run "rejects_wildcard_account" {
  command = plan

  variables {
    database_password_secret_arn = "arn:aws:secretsmanager:us-east-1:*:secret:cudly-dev-db"
  }

  expect_failures = [var.database_password_secret_arn]
}

run "rejects_non_secretsmanager_arn" {
  command = plan

  variables {
    database_password_secret_arn = "arn:aws:ssm:us-east-1:123456789012:parameter/cudly-dev-db"
  }

  expect_failures = [var.database_password_secret_arn]
}
