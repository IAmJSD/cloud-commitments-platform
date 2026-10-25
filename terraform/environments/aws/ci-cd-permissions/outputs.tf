output "role_arn" {
  description = "ARN of the CUDly Terraform deploy role — use as AWS_ROLE_TO_ASSUME in GitHub Actions"
  value       = aws_iam_role.cudly_deploy.arn
}

output "role_name" {
  description = "Name of the CUDly Terraform deploy role"
  value       = aws_iam_role.cudly_deploy.name
}

output "policy_arns" {
  # One entry per aws_iam_role_policy_attachment in role.tf, keyed by the
  # attachment's name; outputs_guard_test.go fails if the two drift (#309).
  # workload_boundary is not here: it is a permissions boundary for the roles
  # the deploy role creates, not a policy attached to the deploy role.
  description = "ARNs of all managed policies attached to the deploy role, keyed by attachment name"
  value = {
    networking = aws_iam_policy.networking.arn
    compute    = aws_iam_policy.compute.arn
    compute_b  = aws_iam_policy.compute_b.arn
    data       = aws_iam_policy.data.arn
    iam        = aws_iam_policy.iam.arn
    monitoring = aws_iam_policy.monitoring.arn
  }
}

output "oidc_provider_arn" {
  description = "ARN of the GitHub Actions OIDC provider (null if github_repo is empty)"
  value       = var.github_repo != "" ? aws_iam_openid_connect_provider.github[0].arn : null
}
