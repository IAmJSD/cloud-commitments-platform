# ==============================================
# Post-deploy Validation Checks
# ==============================================
#
# Runs 13 check blocks during plan/apply (health, headers, public endpoints,
# auth enforcement, errors, methods, content-type, size limits, frontend)
# plus a local-exec provisioner for tests that need TLS introspection or
# multi-step auth flows. See modules/deployment-checks/ for details.

module "deployment_checks" {
  source = "../../modules/deployment-checks"

  api_base_url = trimsuffix(
    var.compute_platform == "lambda"
    ? one(module.compute_lambda[*].function_url)
    : one(module.compute_fargate[*].api_url),
    "/"
  )

  provider_name = "aws"
}

# ==============================================
# Configuration Guards (#128)
# ==============================================
#
# `check` block, not a `precondition`: it runs on every plan/apply and
# surfaces as a WARNING, but never fails the plan or blocks an apply. A
# blocking precondition would also block every future prod deploy (any
# unrelated change) until the CloudFront OAC cutover (#16) lands -- a
# coordinated runtime cutover (DNS + certificate + a human-run apply) this
# change does not perform. Mirrors
# terraform/environments/gcp/checks.tf's identical guard for the GCP half
# of the same defect.
#
# Today this assert fails (WARN) for prod: environment = "prod" has
# enable_cdn = false, so lambda_function_url_auth_type = "NONE" (compute.tf)
# leaves the Lambda Function URL reachable from the open internet with no
# IAM gate. That is the current, tracked state (#128, LeanerCloud/cloud-commitments-cli#575),
# not a regression introduced here -- the point of this check is to make
# sure it cannot become "current" again silently once the #16 cutover lands
# and someone edits a tfvars file back.

check "prod_requires_network_authenticated_ingress" {
  assert {
    condition     = !(var.environment == "prod" && !var.enable_cdn)
    error_message = <<-EOT
      environment = "prod" with enable_cdn = false leaves
      lambda_function_url_auth_type = "NONE" (see compute.tf): the Lambda
      Function URL is reachable from the open internet with no IAM gate.
      This must be resolved by the enable_cdn = true cutover (CloudFront +
      OAC, tracked by #16), not by silencing this warning.
    EOT
  }
}

# ==============================================
# Fargate ALB must not bypass CloudFront (#412)
# ==============================================
#
# With compute_platform = "fargate" and enable_cdn = true, CloudFront fronts
# the ALB (frontend.tf), but the ALB is internet-facing: unless its security
# group admits only CloudFront's origin-facing prefix list, clients can skip
# the distribution (and any WAF on it) by calling the ALB directly.
# networking.tf restricts the group in exactly that case; this check asserts
# the outcome from the group's actual ingress rules
# (module.networking.alb_ingress_cloudfront_only), so a rule added back that
# reopens the ALB warns on every plan. Non-blocking `check`, per #128's
# convention above. Passes today: no environment sets enable_cdn = true.

check "fargate_cdn_requires_cloudfront_only_alb" {
  assert {
    condition     = !(var.compute_platform == "fargate" && var.enable_cdn) || module.networking.alb_ingress_cloudfront_only
    error_message = <<-EOT
      compute_platform = "fargate" with enable_cdn = true, but the ALB
      security group admits traffic from sources other than the CloudFront
      origin-facing managed prefix list: clients can reach the ALB directly
      and bypass CloudFront (and any WAF attached to it). See #412 and
      restrict_alb_ingress_to_cloudfront in modules/networking/aws.
    EOT
  }
}

# ==============================================
# DB Connection Budget (#718)
# ==============================================
#
# A warning, not a failure: with lambda_reserved_concurrency = -1 nothing caps
# how many execution environments, each holding its own DB pool, run at once,
# so a burst can exhaust RDS connections. Reserving concurrency can fail on
# accounts with a low concurrency quota, so dev and staging keep -1 and get
# this warning; lambda_reserved_concurrency's validation makes prod set a cap.
check "lambda_concurrency_bounds_db_connections" {
  assert {
    condition     = !(var.compute_platform == "lambda" && var.lambda_reserved_concurrency < 0)
    error_message = <<-EOT
      lambda_reserved_concurrency = -1: Lambda concurrency is unbounded, so
      concurrent execution environments x db_max_connections can exceed the
      RDS max_connections (database_max_connections) under a burst. Set
      lambda_reserved_concurrency so that it x db_max_connections stays
      within database_max_connections minus the reserved connections.
    EOT
  }
}
