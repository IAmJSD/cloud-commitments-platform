# ==============================================
# Failure alerts (issue #714)
# ==============================================
#
# The application logs a stable marker token for the two failures operators
# must hear about. Each becomes a log metric filter plus an alarm on any
# occurrence, notifying the alerts SNS topic:
#
#   purchase_execution_failed  an execution row ended failed or
#                              partially_completed
#                              (internal/purchase/failure_notify.go)
#   scheduled_task_failed      a scheduled task returned an error, including a
#                              process_scheduled_purchases run with failures
#                              (internal/server/scheduled_tasks.go)
#
# The tokens are a contract with the Go code; TestMonitoringLogMarkersMatchGo
# (terraform/modules/monitoring/log_marker_contract_test.go) fails if either
# side renames one. A quoted term is a substring match anywhere in the event,
# so the timestamp/level prefix the logger adds does not matter.

locals {
  failure_markers = {
    purchase_failed = {
      token       = "purchase_execution_failed"
      metric      = "PurchaseExecutionFailed"
      description = "A purchase execution ended failed or partially_completed. Search the log group for purchase_execution_failed to find the execution_id, then check History."
    }
    scheduled_task_failed = {
      token       = "scheduled_task_failed"
      metric      = "ScheduledTaskFailed"
      description = "A scheduled task (recommendation collection, purchase processing, notifications, ...) returned an error. Search the log group for scheduled_task_failed for the task and error."
    }
  }
}

resource "aws_cloudwatch_log_metric_filter" "failure_marker" {
  for_each = local.failure_markers

  name           = "${var.stack_name}-${replace(each.key, "_", "-")}"
  log_group_name = var.log_group_name
  pattern        = "\"${each.value.token}\""

  metric_transformation {
    name = each.value.metric
    # Per stack: dev and prod can share an account and region, and a shared
    # namespace would let one environment's failures trip the other's alarm.
    namespace     = "CUDly/${var.stack_name}"
    value         = "1"
    default_value = "0"
  }
}

resource "aws_cloudwatch_metric_alarm" "failure_marker" {
  for_each = local.failure_markers

  alarm_name          = "${var.stack_name}-${replace(each.key, "_", "-")}"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 1
  metric_name         = aws_cloudwatch_log_metric_filter.failure_marker[each.key].metric_transformation[0].name
  namespace           = aws_cloudwatch_log_metric_filter.failure_marker[each.key].metric_transformation[0].namespace
  period              = 300
  statistic           = "Sum"
  threshold           = 0
  alarm_description   = each.value.description
  alarm_actions       = [aws_sns_topic.alerts.arn]
  ok_actions          = [aws_sns_topic.alerts.arn]
  treat_missing_data  = "notBreaching"

  tags = var.tags
}
