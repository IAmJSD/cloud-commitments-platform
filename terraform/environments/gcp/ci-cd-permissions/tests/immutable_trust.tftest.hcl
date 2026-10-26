mock_provider "google" {
  mock_resource "google_service_account" {
    defaults = {
      name = "projects/p/serviceAccounts/cudly-terraform-deploy@p.iam.gserviceaccount.com"
    }
  }
}

variables {
  project_id      = "p"
  github_owner_id = "86753534"
  github_repo_id  = "1391259743"
}

run "trust_uses_immutable_ids" {
  command = apply

  assert {
    condition     = google_iam_workload_identity_pool_provider.github[0].attribute_condition == "assertion.repository_owner_id == '86753534' && assertion.repository_id == '1391259743' && assertion.ref == 'refs/heads/main'"
    error_message = "attribute_condition must be exactly owner id, repo id and ref"
  }

  assert {
    condition     = !strcontains(google_iam_workload_identity_pool_provider.github[0].attribute_condition, "assertion.repository ==")
    error_message = "name-only repository clause must not remain"
  }

  assert {
    condition = (
      google_iam_workload_identity_pool_provider.github[0].attribute_mapping["attribute.repository_id"] == "assertion.repository_id"
      && google_iam_workload_identity_pool_provider.github[0].attribute_mapping["attribute.repository_owner_id"] == "assertion.repository_owner_id"
      && google_iam_workload_identity_pool_provider.github[0].attribute_mapping["attribute.repository"] == "assertion.repository"
    )
    error_message = "id mappings must exist and the repository mapping must stay"
  }

  # Issue #323 (A13-014): the grant pins the ref, not just the repo id.
  assert {
    condition     = endswith(google_service_account_iam_member.github_actions[0].member, "/attribute.repository_id_ref/1391259743:refs/heads/main")
    error_message = "member must be keyed on attribute.repository_id_ref with the repo id and deploy_ref"
  }

  assert {
    condition     = !strcontains(google_service_account_iam_member.github_actions[0].member, "/attribute.repository_id/")
    error_message = "the repo-only member (any ref of the repo) must not remain"
  }

  assert {
    condition     = startswith(google_service_account_iam_member.github_actions[0].member, "principalSet://iam.googleapis.com/${google_iam_workload_identity_pool.github[0].name}/")
    error_message = "member must be scoped to this module's pool"
  }

  assert {
    condition     = lookup(google_iam_workload_identity_pool_provider.github[0].attribute_mapping, "attribute.repository_id_ref", "") == "assertion.repository_id + ':' + assertion.ref"
    error_message = "attribute.repository_id_ref must map to the repo id and ref the member pins"
  }

  assert {
    condition     = google_service_account_iam_member.github_actions[0].role == "roles/iam.workloadIdentityUser"
    error_message = "the deploy trust must still grant workloadIdentityUser"
  }
}

run "condition_follows_deploy_ref" {
  command = apply

  variables {
    deploy_ref = "refs/heads/feature/x"
  }

  assert {
    condition     = endswith(google_iam_workload_identity_pool_provider.github[0].attribute_condition, "assertion.ref == 'refs/heads/feature/x'")
    error_message = "condition must follow deploy_ref"
  }

  assert {
    condition     = endswith(google_service_account_iam_member.github_actions[0].member, "/attribute.repository_id_ref/1391259743:refs/heads/feature/x")
    error_message = "member must follow deploy_ref, matching the condition"
  }
}

run "rejects_bad_repo_id" {
  command = plan

  variables {
    github_repo_id = "abc"
  }

  expect_failures = [var.github_repo_id]
}

run "rejects_empty_repo_id" {
  command = plan

  variables {
    github_repo_id = ""
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

run "rejects_non_numeric_owner_id" {
  command = plan

  variables {
    github_owner_id = "octo"
  }

  expect_failures = [var.github_owner_id]
}

run "rejects_empty_owner_id" {
  command = plan

  variables {
    github_owner_id = ""
  }

  expect_failures = [var.github_owner_id]
}

run "rejects_cel_injection_in_deploy_ref" {
  command = plan

  variables {
    deploy_ref = "x' || true"
  }

  expect_failures = [var.deploy_ref]
}

run "rejects_empty_deploy_ref" {
  command = plan

  variables {
    deploy_ref = ""
  }

  expect_failures = [var.deploy_ref]
}

run "rejects_leading_zero_repo_id" {
  command = plan

  variables {
    github_repo_id = "0123"
  }

  expect_failures = [var.github_repo_id]
}

run "rejects_github_repo_with_quote" {
  command = plan

  variables {
    github_repo = "a/b'c"
  }

  expect_failures = [var.github_repo]
}

run "rejects_github_repo_without_slash" {
  command = plan

  variables {
    github_repo = "noslash"
  }

  expect_failures = [var.github_repo]
}

run "skip_mode_creates_no_wif" {
  command = apply

  variables {
    github_repo = ""
  }

  assert {
    condition     = length(google_iam_workload_identity_pool.github) == 0 && length(google_iam_workload_identity_pool_provider.github) == 0 && length(google_service_account_iam_member.github_actions) == 0
    error_message = "empty github_repo must disable WIF resources"
  }
}
