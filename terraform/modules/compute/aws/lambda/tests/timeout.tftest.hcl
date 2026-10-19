# Lambda timeout bounds (#913). Lambda ends an invocation at its timeout
# whatever a context deadline says, so the function must outlive the 270s of
# RI exchange / purchase work detached from the request. The floor is pinned
# against the Go budgets by TestLambdaTimeoutCovers* in internal/api and
# internal/purchase.
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
  database_password_secret_arn = "arn:aws:secretsmanager:us-east-1:123456789012:secret:db"
  admin_email                  = "admin@example.com"
  allowed_origins              = ["https://app.example.com"]
}

run "default_covers_detached_budget" {
  command = plan

  assert {
    condition     = aws_lambda_function.main.timeout == 300
    error_message = "The default Lambda timeout must be 300s (270s detached budget + 30s margin)."
  }
}

run "rejects_old_30s_default" {
  command = plan

  variables {
    timeout = 30
  }

  expect_failures = [var.timeout]
}

run "rejects_just_below_floor" {
  command = plan

  variables {
    timeout = 299
  }

  expect_failures = [var.timeout]
}

run "rejects_above_lambda_max" {
  command = plan

  variables {
    timeout = 901
  }

  expect_failures = [var.timeout]
}

run "accepts_lambda_max" {
  command = plan

  variables {
    timeout = 900
  }

  assert {
    condition     = aws_lambda_function.main.timeout == 900
    error_message = "900s (the Lambda maximum) must be accepted."
  }
}
