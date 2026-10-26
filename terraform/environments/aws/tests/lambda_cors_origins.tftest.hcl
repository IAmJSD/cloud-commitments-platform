# The Function URL's own origin reaches its CORS allowlist from the previous
# apply's output, not from a host pasted into github-dev.tfvars (#324).
#
# Every provider is mocked, so nothing here reaches AWS. Each run targets
# module.compute_lambda, the allowlist's only consumer, and reads the merged
# list from local.lambda_cors_origins, the value compute.tf passes to the
# module's allowed_origins.
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
  admin_email            = "admin@example.com"
  compute_platform       = "lambda"
  enable_docker_build    = false
  image_uri              = "123456789012.dkr.ecr.us-east-1.amazonaws.com/cudly:test"
  lambda_architecture    = "arm64" # skips the builder-host probe (data.external)
  lambda_allowed_origins = ["http://localhost:3000"]
}

run "first_deploy_keeps_static_origins" {
  command = plan

  plan_options {
    target = [module.compute_lambda]
  }

  assert {
    condition     = local.lambda_cors_origins == tolist(["http://localhost:3000"])
    error_message = "With no deployed URL yet, the allowlist must be exactly the tfvars list."
  }
}

run "adds_deployed_function_url_origin" {
  command = plan

  plan_options {
    target = [module.compute_lambda]
  }

  variables {
    lambda_deployed_function_url = "https://abcdefghijklmnopqrstuvwxyz012345.lambda-url.us-east-1.on.aws/"
  }

  assert {
    condition = local.lambda_cors_origins == tolist([
      "http://localhost:3000",
      "https://abcdefghijklmnopqrstuvwxyz012345.lambda-url.us-east-1.on.aws",
    ])
    error_message = "The deployed Function URL's origin (no trailing slash) must be appended to the tfvars list."
  }
}

run "does_not_duplicate_an_origin_already_listed" {
  command = plan

  plan_options {
    target = [module.compute_lambda]
  }

  variables {
    lambda_allowed_origins       = ["https://abcdefghijklmnopqrstuvwxyz012345.lambda-url.us-east-1.on.aws"]
    lambda_deployed_function_url = "https://abcdefghijklmnopqrstuvwxyz012345.lambda-url.us-east-1.on.aws/"
  }

  assert {
    condition     = local.lambda_cors_origins == tolist(["https://abcdefghijklmnopqrstuvwxyz012345.lambda-url.us-east-1.on.aws"])
    error_message = "An origin present in both inputs must appear once."
  }
}

run "rejects_a_non_function_url" {
  command = plan

  plan_options {
    target = [module.compute_lambda]
  }

  variables {
    lambda_deployed_function_url = "https://evil.example.com"
  }

  expect_failures = [var.lambda_deployed_function_url]
}

run "rejects_a_lookalike_host" {
  command = plan

  plan_options {
    target = [module.compute_lambda]
  }

  variables {
    lambda_deployed_function_url = "https://abc.lambda-url.us-east-1.on.aws.evil.example"
  }

  expect_failures = [var.lambda_deployed_function_url]
}
