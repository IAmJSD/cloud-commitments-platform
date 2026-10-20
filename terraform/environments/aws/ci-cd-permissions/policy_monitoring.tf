# Deploy grants for modules/monitoring/aws (issue #714), in their own managed
# policy because aws_iam_policy.compute is at AWS's 6144-char limit.
#
# The environment instantiates that module only when enable_monitoring = true
# (terraform/environments/aws/monitoring.tf), and that flag must stay false
# until this bootstrap has been re-applied: without these grants every apply
# 403s on sns:CreateTopic / cloudwatch:PutDashboard / logs:PutMetricFilter.
# The module's alarms are already covered by CloudWatchAlarms in
# policy_compute.tf (alarm:cudly-*). PutMetricFilter also covers the Lambda
# module's opt-in migration alarm (enable_migration_alarm).
#
# Every resource the module names starts with the stack name ("cudly-<env>-
# <suffix>"), so SNS, dashboards and log groups are ARN-scoped to cudly-*.
# The CloudWatch Logs Insights query-definition and DescribeMetricFilters
# actions do not support resource-level permissions, hence Resource = "*".

resource "aws_iam_policy" "monitoring" {
  name        = "cudly-deploy-monitoring"
  description = "CUDly Terraform deploy: monitoring module (SNS alerts topic, CloudWatch dashboard, log metric filters, Logs Insights queries)"

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid    = "AlertsTopic"
        Effect = "Allow"
        Action = [
          "sns:CreateTopic",
          "sns:DeleteTopic",
          "sns:GetSubscriptionAttributes",
          "sns:GetTopicAttributes",
          "sns:ListSubscriptionsByTopic",
          "sns:ListTagsForResource",
          "sns:SetSubscriptionAttributes",
          "sns:SetTopicAttributes",
          "sns:Subscribe",
          "sns:TagResource",
          "sns:Unsubscribe",
          "sns:UntagResource",
        ]
        Resource = "arn:aws:sns:*:*:cudly-*"
      },
      {
        Sid    = "Dashboard"
        Effect = "Allow"
        Action = [
          "cloudwatch:DeleteDashboards",
          "cloudwatch:GetDashboard",
          "cloudwatch:PutDashboard",
        ]
        Resource = "arn:aws:cloudwatch::*:dashboard/cudly-*"
      },
      {
        Sid    = "LogMetricFilters"
        Effect = "Allow"
        Action = [
          "logs:DeleteMetricFilter",
          "logs:PutMetricFilter",
        ]
        Resource = [
          "arn:aws:logs:*:*:log-group:/aws/lambda/cudly-*",
          "arn:aws:logs:*:*:log-group:/ecs/cudly-*",
        ]
      },
      {
        Sid    = "LogsReadAndQueries"
        Effect = "Allow"
        Action = [
          "logs:DeleteQueryDefinition",
          "logs:DescribeMetricFilters",
          "logs:DescribeQueryDefinitions",
          "logs:PutQueryDefinition",
        ]
        Resource = "*"
      },
    ]
  })

  tags = {
    Project   = "CUDly"
    ManagedBy = "terraform"
  }
}
