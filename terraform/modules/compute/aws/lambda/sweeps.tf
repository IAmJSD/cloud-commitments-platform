# Additional scheduled sweeps. Each entry becomes an EventBridge rule that
# invokes the server task named by the map key. `rule` is the short rule-name
# suffix: rule names are "<stack_name>-sweep-<rule>", so the longest suffix
# (at most 15 chars) fits the 64-char EventBridge limit for any stack_name the
# module accepts (at most 30, set by the IAM role name_prefix in main.tf).
# The "sweep-" infix keeps these names clear of the older per-task rules.
# Retries stay at the EventBridge default: a re-run is safe under the per-task advisory lock and row CAS in the server.
locals {
  sweep_tasks = {
    process_scheduled_purchases = { rule = "process", enabled = var.enable_process_scheduled_purchases_schedule, schedule = var.process_scheduled_purchases_schedule }
    send_notifications          = { rule = "notify", enabled = var.enable_send_notifications_schedule, schedule = var.send_notifications_schedule }
    finalize_revocations        = { rule = "finalize", enabled = var.enable_finalize_revocations_schedule, schedule = var.finalize_revocations_schedule }
  }
  active_sweeps = { for k, v in local.sweep_tasks : k => v if v.enabled }
}

resource "aws_cloudwatch_event_rule" "sweep" {
  for_each = local.active_sweeps

  name                = "${var.stack_name}-sweep-${each.value.rule}"
  description         = "Trigger the ${each.key} scheduled task"
  schedule_expression = each.value.schedule

  tags = var.tags
}

resource "aws_cloudwatch_event_target" "sweep" {
  for_each = local.active_sweeps

  rule      = aws_cloudwatch_event_rule.sweep[each.key].name
  target_id = "lambda"
  arn       = aws_lambda_function.main.arn

  input = jsonencode({ action = each.key })
}

resource "aws_lambda_permission" "sweep" {
  for_each = local.active_sweeps

  statement_id  = "AllowExecutionFromEventBridge-${each.key}"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.main.function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.sweep[each.key].arn
}
