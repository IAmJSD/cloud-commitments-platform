# The five EventBridge invoker roles (#292, plus the shared sweeps role of #712). Each may run exactly the module's
# task definition, only in the module's cluster, and may pass the task and
# task-execution roles only to ECS tasks. Without the two conditions an actor
# who can create EventBridge targets in the account can run the task definition
# in any cluster, or hand the task role (which holds the SP/RI purchase grants)
# to any service that accepts a passed role.
#
# command = apply against a mock provider: the policy embeds the cluster, task
# definition and role ARNs, which are unknown at plan. Nothing is created. The
# overrides give those resources distinct, well-formed ARNs: the provider
# validates ARN-typed arguments, and two roles sharing one mock ARN would let
# a PassRole statement naming only one of them pass. Roles not overridden
# (the five invoker roles) share the default, which nothing asserts on.
mock_provider "aws" {
  mock_resource "aws_iam_role" {
    defaults = { arn = "arn:aws:iam::123456789012:role/cudly-prod-abcd1234-fargate-mock" }
  }
}

override_resource {
  target = aws_iam_role.task
  values = { arn = "arn:aws:iam::123456789012:role/cudly-prod-abcd1234-fargate-task" }
}

override_resource {
  target = aws_iam_role.task_execution
  values = { arn = "arn:aws:iam::123456789012:role/cudly-prod-abcd1234-fargate-task-execution" }
}

override_resource {
  target = aws_ecs_cluster.main
  values = { arn = "arn:aws:ecs:us-east-1:123456789012:cluster/cudly-prod-abcd1234-fargate" }
}

override_resource {
  target = aws_ecs_task_definition.main
  values = { arn = "arn:aws:ecs:us-east-1:123456789012:task-definition/cudly-prod-abcd1234-fargate:1" }
}

override_resource {
  target = aws_lb.main
  values = { arn = "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/cudly-prod-abcd1234-fargate/0000000000000000" }
}

override_resource {
  target = aws_lb_target_group.main
  values = { arn = "arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/cudly-prod-abcd1234-fargate/0000000000000000" }
}

variables {
  stack_name                   = "cudly-prod-abcd1234"
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

  enable_cross_account_sts                 = false
  enable_scheduled_tasks                   = true
  enable_ri_exchange_schedule              = true
  enable_ladder_run_schedule               = true
  enable_fire_scheduled_purchases_schedule = true
}

run "invoker_roles_are_scoped" {
  command = apply

  # recommendations
  assert {
    condition = [
      for s in jsondecode(aws_iam_role_policy.eventbridge[0].policy).Statement :
      [s.Resource, try(s.Condition.ArnEquals["ecs:cluster"], null)] if contains(s.Action, "ecs:RunTask")
    ] == [[aws_ecs_task_definition.main.arn, aws_ecs_cluster.main.arn]]
    error_message = "eventbridge: ecs:RunTask must be one statement on the module's task definition, conditioned on ArnEquals ecs:cluster = the module's cluster."
  }
  assert {
    condition = [
      for s in jsondecode(aws_iam_role_policy.eventbridge[0].policy).Statement :
      [toset(s.Resource), try(s.Condition.StringEquals["iam:PassedToService"], null)] if contains(s.Action, "iam:PassRole")
    ] == [[toset([aws_iam_role.task_execution.arn, aws_iam_role.task.arn]), "ecs-tasks.amazonaws.com"]]
    error_message = "eventbridge: iam:PassRole must be one statement on the task and task-execution roles, conditioned on StringEquals iam:PassedToService = ecs-tasks.amazonaws.com."
  }

  # RI exchange
  assert {
    condition = [
      for s in jsondecode(aws_iam_role_policy.eventbridge_ri_exchange[0].policy).Statement :
      [s.Resource, try(s.Condition.ArnEquals["ecs:cluster"], null)] if contains(s.Action, "ecs:RunTask")
    ] == [[aws_ecs_task_definition.main.arn, aws_ecs_cluster.main.arn]]
    error_message = "eventbridge_ri_exchange: ecs:RunTask must be one statement on the module's task definition, conditioned on ArnEquals ecs:cluster = the module's cluster."
  }
  assert {
    condition = [
      for s in jsondecode(aws_iam_role_policy.eventbridge_ri_exchange[0].policy).Statement :
      [toset(s.Resource), try(s.Condition.StringEquals["iam:PassedToService"], null)] if contains(s.Action, "iam:PassRole")
    ] == [[toset([aws_iam_role.task_execution.arn, aws_iam_role.task.arn]), "ecs-tasks.amazonaws.com"]]
    error_message = "eventbridge_ri_exchange: iam:PassRole must be one statement on the task and task-execution roles, conditioned on StringEquals iam:PassedToService = ecs-tasks.amazonaws.com."
  }

  # ladder run
  assert {
    condition = [
      for s in jsondecode(aws_iam_role_policy.eventbridge_ladder_run[0].policy).Statement :
      [s.Resource, try(s.Condition.ArnEquals["ecs:cluster"], null)] if contains(s.Action, "ecs:RunTask")
    ] == [[aws_ecs_task_definition.main.arn, aws_ecs_cluster.main.arn]]
    error_message = "eventbridge_ladder_run: ecs:RunTask must be one statement on the module's task definition, conditioned on ArnEquals ecs:cluster = the module's cluster."
  }
  assert {
    condition = [
      for s in jsondecode(aws_iam_role_policy.eventbridge_ladder_run[0].policy).Statement :
      [toset(s.Resource), try(s.Condition.StringEquals["iam:PassedToService"], null)] if contains(s.Action, "iam:PassRole")
    ] == [[toset([aws_iam_role.task_execution.arn, aws_iam_role.task.arn]), "ecs-tasks.amazonaws.com"]]
    error_message = "eventbridge_ladder_run: iam:PassRole must be one statement on the task and task-execution roles, conditioned on StringEquals iam:PassedToService = ecs-tasks.amazonaws.com."
  }

  # fire scheduled purchases
  assert {
    condition = [
      for s in jsondecode(aws_iam_role_policy.eventbridge_fire_scheduled_purchases[0].policy).Statement :
      [s.Resource, try(s.Condition.ArnEquals["ecs:cluster"], null)] if contains(s.Action, "ecs:RunTask")
    ] == [[aws_ecs_task_definition.main.arn, aws_ecs_cluster.main.arn]]
    error_message = "eventbridge_fire_scheduled_purchases: ecs:RunTask must be one statement on the module's task definition, conditioned on ArnEquals ecs:cluster = the module's cluster."
  }
  assert {
    condition = [
      for s in jsondecode(aws_iam_role_policy.eventbridge_fire_scheduled_purchases[0].policy).Statement :
      [toset(s.Resource), try(s.Condition.StringEquals["iam:PassedToService"], null)] if contains(s.Action, "iam:PassRole")
    ] == [[toset([aws_iam_role.task_execution.arn, aws_iam_role.task.arn]), "ecs-tasks.amazonaws.com"]]
    error_message = "eventbridge_fire_scheduled_purchases: iam:PassRole must be one statement on the task and task-execution roles, conditioned on StringEquals iam:PassedToService = ecs-tasks.amazonaws.com."
  }

  # sweeps (one role for every sweep in sweeps.tf; reap, analytics_collect
  # and finalize_revocations are on by default, so the role exists here)
  assert {
    condition = [
      for s in jsondecode(aws_iam_role_policy.eventbridge_sweeps[0].policy).Statement :
      [s.Resource, try(s.Condition.ArnEquals["ecs:cluster"], null)] if contains(s.Action, "ecs:RunTask")
    ] == [[aws_ecs_task_definition.main.arn, aws_ecs_cluster.main.arn]]
    error_message = "eventbridge_sweeps: ecs:RunTask must be one statement on the module's task definition, conditioned on ArnEquals ecs:cluster = the module's cluster."
  }
  assert {
    condition = [
      for s in jsondecode(aws_iam_role_policy.eventbridge_sweeps[0].policy).Statement :
      [toset(s.Resource), try(s.Condition.StringEquals["iam:PassedToService"], null)] if contains(s.Action, "iam:PassRole")
    ] == [[toset([aws_iam_role.task_execution.arn, aws_iam_role.task.arn]), "ecs-tasks.amazonaws.com"]]
    error_message = "eventbridge_sweeps: iam:PassRole must be one statement on the task and task-execution roles, conditioned on StringEquals iam:PassedToService = ecs-tasks.amazonaws.com."
  }
}
