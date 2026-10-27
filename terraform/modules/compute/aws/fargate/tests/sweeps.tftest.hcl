# Module wiring for the Fargate sweep rules (#712). Defaults and opt-in policy
# match the Lambda module's sweeps.tftest.hcl. Rule -> target key cross-wiring
# is checked by the Go test in terraform/modules/compute; invoker-role scoping
# by eventbridge_invoker_iam.tftest.hcl.
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
  vpc_id                       = "vpc-00000000"
  private_subnet_ids           = ["subnet-00000001", "subnet-00000002"]
  public_subnet_ids            = ["subnet-00000003", "subnet-00000004"]
  alb_security_group_id        = "sg-00000000"
}

run "defaults" {
  command = plan

  assert {
    condition     = toset(keys(aws_cloudwatch_event_rule.sweep)) == toset(["analytics_collect", "finalize_revocations", "reap_stuck_purchases"])
    error_message = "Only reap_stuck_purchases, analytics_collect and finalize_revocations are default-on sweeps; process, send_notifications and cleanup are opt-in."
  }
  assert {
    condition     = length(aws_cloudwatch_event_target.sweep) == 3 && length(aws_iam_role.eventbridge_sweeps) == 1 && length(aws_iam_role_policy.eventbridge_sweeps) == 1
    error_message = "Each active sweep needs a target, and the sweeps need their invoker role and policy."
  }
  assert {
    condition     = length(aws_cloudwatch_event_rule.fire_scheduled_purchases) == 0 && length(aws_cloudwatch_event_rule.ladder_run) == 0 && length(aws_cloudwatch_event_rule.ri_exchange) == 0
    error_message = "fire, ladder_run and ri_exchange stay opt-in."
  }
  assert {
    condition = alltrue([
      for k, r in aws_cloudwatch_event_rule.sweep :
      length(r.name) <= 64 && startswith(r.name, "cudly-") && endswith(r.name, "-sweep-${local.sweep_tasks[k].rule}")
    ])
    error_message = "Rule names must be <= 64 chars, keep the cudly- prefix and end with -sweep-<rule>."
  }
  assert {
    condition     = aws_cloudwatch_event_rule.sweep["reap_stuck_purchases"].schedule_expression == "rate(5 minutes)" && aws_cloudwatch_event_rule.sweep["analytics_collect"].schedule_expression == "rate(1 day)" && aws_cloudwatch_event_rule.sweep["finalize_revocations"].schedule_expression == "rate(1 hour)"
    error_message = "Default schedules must match the Lambda module."
  }
  assert {
    condition = alltrue([
      for k, t in aws_cloudwatch_event_target.sweep :
      t.input == jsonencode({ containerOverrides = [{ name = "app", command = ["./cudly", "--task", k] }] })
    ])
    error_message = "Each target must run ./cudly --task <its task> in the app container."
  }
}

run "opt_in" {
  command = plan

  variables {
    enable_process_scheduled_purchases_schedule = true
    enable_send_notifications_schedule          = true
    enable_cleanup_schedule                     = true
  }

  assert {
    condition     = toset(keys(aws_cloudwatch_event_rule.sweep)) == toset(["analytics_collect", "cleanup", "finalize_revocations", "process_scheduled_purchases", "reap_stuck_purchases", "send_notifications"])
    error_message = "Opting in must add process, send_notifications and cleanup."
  }
  assert {
    condition     = length(aws_cloudwatch_event_target.sweep) == 6 && length(aws_iam_role.eventbridge_sweeps) == 1
    error_message = "Every opted-in sweep needs a target; all share one invoker role."
  }
  assert {
    condition     = length(distinct([for r in aws_cloudwatch_event_rule.sweep : r.name])) == 6
    error_message = "Sweep rule names must be distinct."
  }
  assert {
    condition = alltrue([
      for k, r in aws_cloudwatch_event_rule.sweep :
      length(r.name) <= 64 && r.schedule_expression == local.sweep_tasks[k].schedule
    ])
    error_message = "Rule names must be <= 64 chars and schedules must come from the variables."
  }
  assert {
    condition = alltrue([
      for k, t in aws_cloudwatch_event_target.sweep :
      t.input == jsonencode({ containerOverrides = [{ name = "app", command = ["./cudly", "--task", k] }] })
    ])
    error_message = "Each target must run ./cudly --task <its task> in the app container."
  }
}

run "all_off" {
  command = plan

  variables {
    enable_reap_stuck_purchases_schedule = false
    enable_analytics_collect_schedule    = false
    enable_finalize_revocations_schedule = false
  }

  assert {
    condition     = length(aws_cloudwatch_event_rule.sweep) == 0 && length(aws_iam_role.eventbridge_sweeps) == 0 && length(aws_iam_role_policy.eventbridge_sweeps) == 0
    error_message = "With every sweep off, no rule and no invoker role may be created."
  }
}
