# Pins which GitHub environments get a federated credential on the deploy
# service principal by default (#364). Every name here must meet the gating
# rule on var.github_environments: the environment exists with a deployment
# branch policy that admits only `main`. Changing this set changes who can
# obtain the service principal and needs a bootstrap re-apply, so update the
# variable's description and this test together.

mock_provider "azurerm" {}
mock_provider "azuread" {}

variables {
  subscription_id = "00000000-0000-0000-0000-000000000000"
  github_owner_id = "86753534"
  github_repo_id  = "1391259743"
}

run "default_environments_are_the_gated_plain_names" {
  command = plan

  assert {
    condition     = toset(keys(azuread_application_federated_identity_credential.github_environment)) == toset(["dev", "staging", "prod"])
    error_message = "default github_environments must be exactly dev, staging and prod; a new name needs an existing environment with a main-only branch policy first (see var.github_environments)"
  }

  assert {
    condition     = try(azuread_application_federated_identity_credential.github_environment["prod"].subject, "") == "repo:LeanerCloud@86753534/cloud-commitments-platform@1391259743:environment:prod"
    error_message = "prod credential must trust the plain prod environment subject that deploy-azure.yml and rollback.yml present"
  }

  assert {
    condition     = azuread_application_federated_identity_credential.github_main[0].subject == "repo:LeanerCloud@86753534/cloud-commitments-platform@1391259743:ref:refs/heads/main"
    error_message = "the main-branch subject is what makes a main-only environment credential add no reach; it must stay"
  }
}

run "no_environment_credentials_without_repo" {
  command = plan

  variables {
    github_repo = ""
  }

  assert {
    condition     = length(azuread_application_federated_identity_credential.github_environment) == 0
    error_message = "an empty github_repo must create no environment credentials"
  }
}

run "all_subjects_use_immutable_ids" {
  command = plan

  assert {
    condition = alltrue(concat(
      [for c in azuread_application_federated_identity_credential.github_main : startswith(c.subject, "repo:LeanerCloud@86753534/cloud-commitments-platform@1391259743:")],
      [for c in azuread_application_federated_identity_credential.github_environment : startswith(c.subject, "repo:LeanerCloud@86753534/cloud-commitments-platform@1391259743:environment:")],
    )) && length(azuread_application_federated_identity_credential.github_environment) == 3
    error_message = "every federated credential subject must use the immutable-id prefix; no name-only subject may remain"
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
