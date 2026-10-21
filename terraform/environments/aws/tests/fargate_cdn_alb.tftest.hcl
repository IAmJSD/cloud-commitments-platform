# Fargate ALB behind CloudFront (#412). With compute_platform = "fargate" and
# enable_cdn = true, the environment must restrict the ALB security group to
# CloudFront's origin-facing prefix list, and check
# "fargate_cdn_requires_cloudfront_only_alb" must stay quiet. Everything but
# module.networking is overridden, so this plans offline with mock providers.
mock_provider "aws" {
  mock_data "aws_ec2_managed_prefix_list" {
    defaults = {
      id = "pl-3b927c52"
    }
  }

  mock_data "aws_availability_zones" {
    defaults = {
      names = ["us-east-1a", "us-east-1b", "us-east-1c"]
    }
  }

  # main.tf builds permissions_boundary_arn from this, and the provider
  # rejects an ARN whose account is not 12 digits. Synthetic placeholder.
  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "123456789012"
    }
  }
}

mock_provider "random" {}
mock_provider "http" {}
mock_provider "external" {
  mock_data "external" {
    defaults = {
      result = { arch = "arm64" }
    }
  }
}

override_module {
  target = module.build
}

override_module {
  target = module.registry
}

# outputs.tf interpolates these into a heredoc, which rejects nulls.
override_module {
  target = module.database
  outputs = {
    database_name        = "cudly"
    instance_address     = "db.example.com"
    proxy_endpoint       = "proxy.example.com"
    password_secret_name = "cudly-test-db-password"
  }
}

override_module {
  target = module.secrets
}

override_module {
  target = module.compute_lambda
  outputs = {
    function_name  = "cudly-test-api"
    function_url   = "https://abc123.lambda-url.us-east-1.on.aws/"
    log_group_name = "/aws/lambda/cudly-test-api"
  }
}

override_module {
  target = module.compute_fargate
  outputs = {
    alb_dns_name = "cudly-test-alb.us-east-1.elb.amazonaws.com"
    api_url      = "http://cudly-test-alb.us-east-1.elb.amazonaws.com"
  }
}

override_module {
  target = module.frontend
  outputs = {
    frontend_url = "https://app.example.com"
  }
}

override_module {
  target = module.deployment_checks
}

variables {
  environment         = "dev"
  region              = "us-east-1"
  compute_platform    = "fargate"
  enable_docker_build = false
  image_uri           = "123456789012.dkr.ecr.us-east-1.amazonaws.com/cudly:test"
  admin_email         = "admin@example.com"
}

run "fargate_with_cdn_restricts_alb_to_cloudfront" {
  command = plan

  variables {
    enable_cdn = true
  }

  assert {
    condition     = module.networking.alb_ingress_cloudfront_only
    error_message = "compute_platform = fargate with enable_cdn = true must leave the ALB admitting only CloudFront's origin-facing prefix list."
  }
}

run "fargate_without_cdn_keeps_alb_public" {
  command = plan

  variables {
    enable_cdn = false
  }

  assert {
    condition     = !module.networking.alb_ingress_cloudfront_only
    error_message = "Without a CDN the ALB is the public endpoint and must stay internet-facing."
  }
}

run "lambda_with_cdn_does_not_trip_the_fargate_check" {
  command = plan

  # A positive lambda_reserved_concurrency keeps check
  # "lambda_concurrency_bounds_db_connections" (#718) quiet: its default of -1
  # trips that check, and a failed check fails the run. 20 x the default
  # db_max_connections (3) fits database_max_connections (80) minus 10 reserved.
  variables {
    compute_platform            = "lambda"
    enable_cdn                  = true
    lambda_reserved_concurrency = 20
  }

  assert {
    condition     = module.networking.alb_security_group_id == null && !module.networking.alb_ingress_cloudfront_only
    error_message = "Lambda creates no ALB group; the Fargate check must not fire for it."
  }
}
