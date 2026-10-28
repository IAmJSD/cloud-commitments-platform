# ==============================================
# Networking
# ==============================================

module "networking" {
  source = "../../modules/networking/aws"

  stack_name               = local.stack_name
  region                   = var.region
  permissions_boundary_arn = local.permissions_boundary_arn

  vpc_cidr                  = var.vpc_cidr
  az_count                  = var.az_count
  create_alb_security_group = var.compute_platform == "fargate"
  enable_flow_logs          = var.enable_flow_logs
  flow_logs_retention_days  = var.flow_logs_retention_days
  enable_nat_gateway        = true # Required for ECR access from private subnets

  # When CloudFront fronts the Fargate ALB (enable_cdn = true), admit only
  # CloudFront's origin-facing prefix list so direct internet traffic cannot
  # bypass the distribution (#412). Mirrors how enable_cdn flips the Lambda
  # Function URL to AWS_IAM (compute.tf). Every committed tfvars sets
  # enable_cdn = false, so this changes nothing on today's stacks.
  restrict_alb_ingress_to_cloudfront = var.compute_platform == "fargate" && var.enable_cdn

  tags = local.common_tags
}
