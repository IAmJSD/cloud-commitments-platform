# Issue #714: no alert reached anyone. A failed purchase or scheduled task must
# trip an alarm that notifies the alerts topic, the topic must have one email
# subscription per configured address, and the database alarms must watch the
# RDS instance the database module actually creates.
mock_provider "aws" {
  # The provider validates alarm actions as ARNs; the mock's random strings fail.
  mock_resource "aws_sns_topic" {
    defaults = {
      arn = "arn:aws:sns:us-east-1:111111111111:cudly-test-abcd1234-alerts"
    }
  }
}

variables {
  stack_name             = "cudly-test-abcd1234"
  environment            = "dev"
  aws_region             = "us-east-1"
  compute_platform       = "lambda"
  function_name          = "cudly-test-api"
  db_instance_identifier = "cudly-test-db"
  log_group_name         = "/aws/lambda/cudly-test-api-0001"
  alert_email_addresses  = ["alerts-a@example.com", "alerts-b@example.com"]
}

run "failure_markers_alarm_on_any_occurrence" {
  command = plan

  assert {
    condition     = aws_cloudwatch_log_metric_filter.failure_marker["purchase_failed"].pattern == "\"purchase_execution_failed\""
    error_message = "The purchase-failure filter must match the purchase_execution_failed marker the purchase manager logs."
  }

  assert {
    condition     = aws_cloudwatch_log_metric_filter.failure_marker["scheduled_task_failed"].pattern == "\"scheduled_task_failed\""
    error_message = "The scheduled-task filter must match the scheduled_task_failed marker the server logs."
  }

  assert {
    condition = alltrue([
      for f in aws_cloudwatch_log_metric_filter.failure_marker : f.log_group_name == var.log_group_name
    ])
    error_message = "The failure filters must read the application's log group."
  }

  assert {
    condition = alltrue([
      for a in aws_cloudwatch_metric_alarm.failure_marker :
      a.threshold == 0 && a.comparison_operator == "GreaterThanThreshold" && a.statistic == "Sum" &&
      a.treat_missing_data == "notBreaching"
    ])
    error_message = "A single failure in the period must trip the alarm, and quiet periods must not."
  }

  assert {
    condition = alltrue([
      for k, a in aws_cloudwatch_metric_alarm.failure_marker :
      a.metric_name == aws_cloudwatch_log_metric_filter.failure_marker[k].metric_transformation[0].name &&
      a.namespace == aws_cloudwatch_log_metric_filter.failure_marker[k].metric_transformation[0].namespace
    ])
    error_message = "Each alarm must watch the metric its own filter emits."
  }

  assert {
    condition = alltrue([
      for f in aws_cloudwatch_log_metric_filter.failure_marker :
      f.metric_transformation[0].namespace == "CUDly/${var.stack_name}"
    ])
    error_message = "Failure metrics must be namespaced per stack so one environment cannot trip another's alarm."
  }
}

run "alarms_notify_the_alerts_topic" {
  command = apply

  assert {
    condition = alltrue([
      for a in aws_cloudwatch_metric_alarm.failure_marker :
      a.alarm_actions == toset([aws_sns_topic.alerts.arn])
    ])
    error_message = "The failure alarms must notify the alerts SNS topic."
  }

  assert {
    condition     = length(aws_sns_topic_subscription.email) == 2
    error_message = "Each configured alert address needs its own email subscription."
  }

  assert {
    condition = (
      toset([for s in aws_sns_topic_subscription.email : s.endpoint]) == toset(["alerts-a@example.com", "alerts-b@example.com"]) &&
      alltrue([for s in aws_sns_topic_subscription.email : s.protocol == "email" && s.topic_arn == aws_sns_topic.alerts.arn])
    )
    error_message = "The email subscriptions must target the alerts topic with the configured addresses."
  }
}

run "db_alarms_watch_the_rds_instance" {
  command = plan

  assert {
    condition = alltrue([
      for a in [aws_cloudwatch_metric_alarm.db_cpu, aws_cloudwatch_metric_alarm.db_connections] :
      length(a.dimensions) == 1 && lookup(a.dimensions, "DBInstanceIdentifier", "") == "cudly-test-db"
    ])
    error_message = "modules/database/aws runs an aws_db_instance; a DBClusterIdentifier dimension never receives data."
  }
}

run "error_filter_matches_the_logger_format" {
  command = plan

  assert {
    condition     = aws_cloudwatch_log_metric_filter.errors.pattern == "\"[ERROR]\""
    error_message = "The app logs \"<date> <time> [ERROR] msg\"; the error filter must match that literal."
  }
}

# The dashboard picks per-platform widgets whose tuples differ in length; both
# platforms must plan (the lambda branch used to fail with a type mismatch).
run "fargate_platform_plans" {
  command = plan

  variables {
    compute_platform = "fargate"
    function_name    = ""
    log_group_name   = "/ecs/cudly-test"
  }

  assert {
    condition     = length(aws_cloudwatch_metric_alarm.lambda_errors) == 0 && length(aws_cloudwatch_metric_alarm.failure_marker) == 2
    error_message = "Fargate stacks get the failure alarms but no Lambda alarms."
  }
}
