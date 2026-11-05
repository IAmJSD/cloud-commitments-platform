# The deploy role trusts GitHub's immutable OIDC subject
# (repo:<owner>@<owner_id>/<name>@<repo_id>), matching the repo's
# use_immutable_subject customization. A name-only subject would stop matching
# live tokens and, worse, trust a renamed or re-created repo of the same name.

mock_provider "aws" {
  mock_resource "aws_iam_policy" {
    defaults = {
      arn = "arn:aws:iam::aws:policy/mock"
    }
  }

  mock_resource "aws_iam_openid_connect_provider" {
    defaults = {
      arn = "arn:aws:iam::aws:oidc-provider/token.actions.githubusercontent.com"
    }
  }

  mock_resource "aws_iam_role" {
    defaults = {
      arn = "arn:aws:iam::aws:role/mock"
    }
  }
}

variables {
  github_owner_id = "86753534"
  github_repo_id  = "1391259743"
}

run "subjects_use_immutable_ids" {
  command = apply

  assert {
    condition = toset([for s in jsondecode(aws_iam_role.cudly_deploy.assume_role_policy).Statement : s if s.Action == "sts:AssumeRoleWithWebIdentity"][0].Condition.StringEquals["token.actions.githubusercontent.com:sub"]) == toset([
      "repo:LeanerCloud@86753534/cloud-commitments-platform@1391259743:ref:refs/heads/main",
      "repo:LeanerCloud@86753534/cloud-commitments-platform@1391259743:environment:dev",
      "repo:LeanerCloud@86753534/cloud-commitments-platform@1391259743:environment:staging",
      "repo:LeanerCloud@86753534/cloud-commitments-platform@1391259743:environment:prod",
      "repo:LeanerCloud@86753534/cloud-commitments-platform@1391259743:environment:aws-fargate-dev",
      "repo:LeanerCloud@86753534/cloud-commitments-platform@1391259743:environment:aws-fargate-staging",
      "repo:LeanerCloud@86753534/cloud-commitments-platform@1391259743:environment:aws-fargate-prod",
      "repo:LeanerCloud@86753534/cloud-commitments-platform@1391259743:environment:aws-db-dev",
      "repo:LeanerCloud@86753534/cloud-commitments-platform@1391259743:environment:aws-db-staging",
      "repo:LeanerCloud@86753534/cloud-commitments-platform@1391259743:environment:aws-db-prod",
    ]) && length([for s in jsondecode(aws_iam_role.cudly_deploy.assume_role_policy).Statement : s if s.Action == "sts:AssumeRoleWithWebIdentity"][0].Condition.StringEquals["token.actions.githubusercontent.com:sub"]) == 10
    error_message = "trust subjects must be exactly the immutable-id main ref and environment subjects"
  }

  assert {
    condition     = alltrue([for s in [for s in jsondecode(aws_iam_role.cudly_deploy.assume_role_policy).Statement : s if s.Action == "sts:AssumeRoleWithWebIdentity"][0].Condition.StringEquals["token.actions.githubusercontent.com:sub"] : !startswith(s, "repo:LeanerCloud/cloud-commitments-platform:")])
    error_message = "no name-only subject may remain"
  }
}

run "subjects_follow_the_ids" {
  command = apply

  variables {
    github_repo     = "acme/widgets"
    github_owner_id = "42"
    github_repo_id  = "7"
  }

  assert {
    condition     = alltrue([for s in jsondecode(aws_iam_role.cudly_deploy.assume_role_policy).Statement[0].Condition.StringEquals["token.actions.githubusercontent.com:sub"] : startswith(s, "repo:acme@42/widgets@7:")])
    error_message = "every subject must carry the supplied owner and repo ids"
  }
}

run "rejects_bad_repo_id" {
  command = plan

  variables {
    github_repo_id = "abc"
  }

  expect_failures = [var.github_repo_id]
}

run "rejects_leading_zero_owner_id" {
  command = plan

  variables {
    github_owner_id = "0123"
  }

  expect_failures = [var.github_owner_id]
}

run "no_oidc_trust_without_repo" {
  command = apply

  variables {
    github_repo = ""
  }

  assert {
    condition     = length(aws_iam_openid_connect_provider.github) == 0
    error_message = "empty github_repo must disable the OIDC provider"
  }
}
