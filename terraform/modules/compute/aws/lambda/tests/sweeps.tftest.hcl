# Module wiring for the sweep rules. Cross-wiring (rule -> target -> permission
# keys) cannot be asserted here on Terraform 1.10 (override_resource needs
# 1.11); the Go test in terraform/modules/compute covers it.
mock_provider "aws" {}

variables {
  # A long production-style name; rule names are asserted against the 64-char limit.
  stack_name                   = "cudly-production-abcd1234"
  permissions_boundary_arn     = "arn:aws:iam::123456789012:policy/cudly-boundary"
  environment                  = "production"
  region                       = "us-east-1"
  image_uri                    = "123456789012.dkr.ecr.us-east-1.amazonaws.com/cudly:test"
  database_host                = "db.example.com"
  database_name                = "cudly"
  database_username            = "cudly"
  database_password_secret_arn = "arn:aws:secretsmanager:us-east-1:123456789012:secret:db"
  admin_email                  = "admin@example.com"
  allowed_origins              = ["https://app.example.com"]
}

run "defaults" {
  command = plan

  assert {
    condition     = toset(keys(aws_cloudwatch_event_rule.sweep)) == toset(["finalize_revocations"])
    error_message = "Only finalize_revocations is a default-on sweep (cleanup is opt-in)."
  }
  assert {
    condition     = length(aws_cloudwatch_event_rule.scheduled_recommendations) == 1 && length(aws_cloudwatch_event_rule.reap_stuck_purchases) == 1 && length(aws_cloudwatch_event_rule.analytics_collect) == 1
    error_message = "collect, reap and analytics_collect stay default-on."
  }
  assert {
    condition     = length(aws_cloudwatch_event_target.reap_stuck_purchases) == 1 && length(aws_lambda_permission.eventbridge_reap_stuck_purchases) == 1 && length(aws_cloudwatch_event_target.analytics_collect) == 1 && length(aws_lambda_permission.eventbridge_analytics_collect) == 1
    error_message = "reap and analytics_collect need their target and permission too."
  }
  assert {
    condition     = length(aws_cloudwatch_event_rule.fire_scheduled_purchases) == 1 && length(aws_cloudwatch_event_target.fire_scheduled_purchases) == 1 && length(aws_lambda_permission.eventbridge_fire_scheduled_purchases) == 1
    error_message = "The fire sweep is on by default (#712), with its target and permission."
  }
  assert {
    condition     = aws_cloudwatch_event_rule.fire_scheduled_purchases[0].schedule_expression == "rate(15 minutes)"
    error_message = "The fire sweep runs every 15 minutes by default."
  }
  assert {
    condition     = length(aws_cloudwatch_event_rule.ladder_run) == 0 && length(aws_cloudwatch_event_rule.ri_exchange) == 0
    error_message = "ladder_run and ri_exchange stay opt-in."
  }
  assert {
    condition     = aws_cloudwatch_event_rule.scheduled_recommendations[0].schedule_expression == "cron(0 2 * * ? *)"
    error_message = "Recommendations are collected daily at 02:00 UTC by default, on a cron the API can report next_collection_at for (#756)."
  }
  assert {
    condition = alltrue([
      for k, r in aws_cloudwatch_event_rule.sweep :
      length(r.name) <= 64 && startswith(r.name, "cudly-") && endswith(r.name, "-sweep-${ { process_scheduled_purchases = "process", send_notifications = "notify", finalize_revocations = "finalize", cleanup = "cleanup" }[k]}")
    ])
    error_message = "Rule names must be <= 64 chars, keep the cudly- prefix and end with -sweep-<rule>."
  }
  assert {
    condition     = length(distinct([for r in aws_cloudwatch_event_rule.sweep : r.name])) == length(aws_cloudwatch_event_rule.sweep)
    error_message = "Sweep rule names must be distinct."
  }
  assert {
    condition     = length(aws_cloudwatch_event_target.sweep) == 1 && length(aws_lambda_permission.sweep) == 1
    error_message = "Each active sweep needs a target and a permission."
  }
  assert {
    condition     = aws_cloudwatch_event_rule.sweep["finalize_revocations"].schedule_expression == var.finalize_revocations_schedule
    error_message = "Schedule must come from the variable."
  }
  assert {
    condition     = aws_cloudwatch_event_target.sweep["finalize_revocations"].input == jsonencode({ action = "finalize_revocations" })
    error_message = "Target input must name the task."
  }
}

run "opt_in" {
  command = plan

  variables {
    enable_process_scheduled_purchases_schedule = true
    enable_send_notifications_schedule          = true
    enable_fire_scheduled_purchases_schedule    = true
    enable_cleanup_schedule                     = true
  }

  assert {
    condition     = toset(keys(aws_cloudwatch_event_rule.sweep)) == toset(["cleanup", "finalize_revocations", "process_scheduled_purchases", "send_notifications"])
    error_message = "Opting in must add process, send_notifications and cleanup."
  }
  assert {
    condition     = length(aws_cloudwatch_event_rule.fire_scheduled_purchases) == 1
    error_message = "Fire stays on its own resource and appears when enabled."
  }
  assert {
    condition = alltrue([
      for k, r in aws_cloudwatch_event_rule.sweep :
      length(r.name) <= 64 && startswith(r.name, "cudly-") && endswith(r.name, "-sweep-${ { process_scheduled_purchases = "process", send_notifications = "notify", finalize_revocations = "finalize", cleanup = "cleanup" }[k]}")
    ])
    error_message = "Rule names must be <= 64 chars, keep the cudly- prefix and end with -sweep-<rule>."
  }
  assert {
    condition     = length(distinct([for r in aws_cloudwatch_event_rule.sweep : r.name])) == length(aws_cloudwatch_event_rule.sweep)
    error_message = "Sweep rule names must be distinct."
  }
  assert {
    condition     = length(aws_cloudwatch_event_target.sweep) == 4
    error_message = "Every opted-in sweep needs a target."
  }
  assert {
    condition = alltrue([
      for k, r in aws_cloudwatch_event_rule.sweep :
      r.schedule_expression == { process_scheduled_purchases = var.process_scheduled_purchases_schedule, send_notifications = var.send_notifications_schedule, finalize_revocations = var.finalize_revocations_schedule, cleanup = var.cleanup_schedule }[k]
    ])
    error_message = "Schedule must come from the variable."
  }
  assert {
    condition = alltrue([
      for k, t in aws_cloudwatch_event_target.sweep : t.input == jsonencode({ action = k })
    ])
    error_message = "Each target input must name its task."
  }
}

run "fire_opt_out" {
  command = plan

  variables {
    enable_fire_scheduled_purchases_schedule = false
  }

  assert {
    condition     = length(aws_cloudwatch_event_rule.fire_scheduled_purchases) == 0 && length(aws_cloudwatch_event_target.fire_scheduled_purchases) == 0 && length(aws_lambda_permission.eventbridge_fire_scheduled_purchases) == 0
    error_message = "Setting enable_fire_scheduled_purchases_schedule = false must remove the fire rule, target and permission."
  }
}
