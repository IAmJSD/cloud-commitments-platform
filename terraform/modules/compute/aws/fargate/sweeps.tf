# Additional scheduled sweeps (#712). Each enabled entry becomes an
# EventBridge rule whose target runs a one-off copy of the app task with
# `./cudly --task <map key>`, the same entry point the recommendations, RI
# exchange, ladder and fire targets in main.tf use. Task names, schedule
# variables and enable defaults match the Lambda module: reap_stuck_purchases,
# analytics_collect and finalize_revocations are on by default;
# process_scheduled_purchases, send_notifications and cleanup are opt-in
# (default off), so no deployment starts proposing purchases, sending
# notification emails or deleting execution history unless it asks to.
#
# `rule` is the rule-name suffix: rule names are
# "<stack_name>-fargate-sweep-<rule>" (longest suffix "-sweep-analytics").
# Retries stay off, as on the other ECS targets in main.tf; a missed run is
# picked up by the next tick, and an overlapping run is a no-op under the
# server's per-task advisory lock.
locals {
  sweep_tasks = {
    reap_stuck_purchases        = { rule = "reap", enabled = var.enable_reap_stuck_purchases_schedule, schedule = var.reap_stuck_purchases_schedule }
    analytics_collect           = { rule = "analytics", enabled = var.enable_analytics_collect_schedule, schedule = var.analytics_collect_schedule }
    process_scheduled_purchases = { rule = "process", enabled = var.enable_process_scheduled_purchases_schedule, schedule = var.process_scheduled_purchases_schedule }
    send_notifications          = { rule = "notify", enabled = var.enable_send_notifications_schedule, schedule = var.send_notifications_schedule }
    finalize_revocations        = { rule = "finalize", enabled = var.enable_finalize_revocations_schedule, schedule = var.finalize_revocations_schedule }
    cleanup                     = { rule = "cleanup", enabled = var.enable_cleanup_schedule, schedule = var.cleanup_schedule }
  }
  active_sweeps = { for k, v in local.sweep_tasks : k => v if v.enabled }
}

resource "aws_cloudwatch_event_rule" "sweep" {
  for_each = local.active_sweeps

  name                = "${local.name_prefix}-sweep-${each.value.rule}"
  description         = "Trigger the ${each.key} scheduled task"
  schedule_expression = each.value.schedule

  tags = local.common_tags
}

resource "aws_cloudwatch_event_target" "sweep" {
  for_each = local.active_sweeps

  rule      = aws_cloudwatch_event_rule.sweep[each.key].name
  target_id = "ecs-task"
  arn       = aws_ecs_cluster.main.arn
  role_arn  = aws_iam_role.eventbridge_sweeps[0].arn

  ecs_target {
    task_count          = 1
    task_definition_arn = aws_ecs_task_definition.main.arn
    launch_type         = "FARGATE"
    platform_version    = "LATEST"

    network_configuration {
      subnets          = var.private_subnet_ids
      security_groups  = [aws_security_group.ecs_tasks.id]
      assign_public_ip = false
    }
  }

  retry_policy {
    maximum_retry_attempts       = 0
    maximum_event_age_in_seconds = 3600
  }

  input = jsonencode({
    containerOverrides = [{
      name    = "app"
      command = ["./cudly", "--task", each.key]
    }]
  })
}

# One invoker role for all sweeps: every sweep runs the same task definition
# in the same cluster, so per-task roles would carry identical policies. Same
# scoping as the other invoker roles in main.tf (#292): RunTask only on the
# module's task definition in the module's cluster, PassRole only for the
# task and task-execution roles and only to ECS tasks.
resource "aws_iam_role" "eventbridge_sweeps" {
  count = length(local.active_sweeps) > 0 ? 1 : 0

  name                 = "${local.name_prefix}-eb-sweeps"
  permissions_boundary = var.permissions_boundary_arn

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Principal = {
          Service = "events.amazonaws.com"
        }
        Action = "sts:AssumeRole"
      }
    ]
  })

  tags = local.common_tags
}

resource "aws_iam_role_policy" "eventbridge_sweeps" {
  count = length(local.active_sweeps) > 0 ? 1 : 0

  name = "ecs-run-task"
  role = aws_iam_role.eventbridge_sweeps[0].id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = [
          "ecs:RunTask"
        ]
        Resource = aws_ecs_task_definition.main.arn
        Condition = {
          ArnEquals = {
            "ecs:cluster" = aws_ecs_cluster.main.arn
          }
        }
      },
      {
        Effect = "Allow"
        Action = [
          "iam:PassRole"
        ]
        Resource = [
          aws_iam_role.task_execution.arn,
          aws_iam_role.task.arn
        ]
        Condition = {
          StringEquals = {
            "iam:PassedToService" = "ecs-tasks.amazonaws.com"
          }
        }
      }
    ]
  })
}
