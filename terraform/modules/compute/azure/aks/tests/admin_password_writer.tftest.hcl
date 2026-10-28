# The AKS deployment sets ADMIN_PASSWORD_SECRET and ADMIN_EMAIL, which arms the
# app's admin-password sync (azsecrets.SetSecret, internal/server/app.go). The
# workload identity's vault-wide grant is the read-only "Key Vault Secrets
# User", so it needs a write grant on that one secret and nowhere else (#199).
#
# Every run is a targeted plan. Under mock providers the cluster's
# kubelet_identity block (Optional+Computed) plans as an empty list, so
# aks_acr's kubelet_identity[0] cannot be evaluated, and apply-time teardown
# re-plans the whole module. Targeting keeps aks_acr out of the graph. The
# grant's principal_id is therefore unknown here and not asserted; the
# container-apps module's test applies and checks the same binding pattern.
mock_provider "azurerm" {}
mock_provider "kubernetes" {}
mock_provider "helm" {}
mock_provider "time" {}

variables {
  project_name                  = "cudly"
  environment                   = "dev"
  resource_group_name           = "cudly-test-rg"
  location                      = "eastus2"
  vnet_subnet_id                = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/cudly-test-rg/providers/Microsoft.Network/virtualNetworks/vnet/subnets/aks"
  image_name                    = "cudlytest.azurecr.io/cudly"
  database_host                 = "cudly-test-postgres.postgres.database.azure.com"
  database_name                 = "cudly"
  database_username             = "cudlyadmin"
  database_password_secret_name = "database-password"
  key_vault_id                  = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/cudly-test-rg/providers/Microsoft.KeyVault/vaults/cudly-test-kv"
  admin_email                   = "admin@example.com"
  admin_password_secret_name    = "admin-password"
}

run "sync_armed_grants_write_on_that_secret_only" {
  command = plan

  plan_options {
    target = [
      azurerm_role_assignment.workload_admin_password_secret_writer,
      azurerm_role_assignment.workload_kv_secrets_user,
    ]
  }

  assert {
    condition     = length(azurerm_role_assignment.workload_admin_password_secret_writer) == 1
    error_message = "With ADMIN_PASSWORD_SECRET and ADMIN_EMAIL both set the sync is armed, so the workload identity needs a write grant or SetSecret 403s."
  }

  assert {
    condition     = azurerm_role_assignment.workload_admin_password_secret_writer[0].scope == "${var.key_vault_id}/secrets/admin-password"
    error_message = "The write grant must be scoped to the admin-password secret itself, not the vault or anything wider."
  }

  assert {
    condition     = azurerm_role_assignment.workload_admin_password_secret_writer[0].role_definition_name == "Key Vault Secrets Officer"
    error_message = "Key Vault Secrets Officer is the built-in role that carries secrets/setSecret/action."
  }

  # Nothing broader: the vault-wide grant stays read-only.
  assert {
    condition     = azurerm_role_assignment.workload_kv_secrets_user.scope == var.key_vault_id && azurerm_role_assignment.workload_kv_secrets_user.role_definition_name == "Key Vault Secrets User"
    error_message = "The vault-wide grant must stay the read-only Key Vault Secrets User; only the single-secret grant may write."
  }
}

# The granted secret must be the one the pods are told to write.
run "granted_secret_is_the_one_the_pods_write" {
  command = plan

  plan_options {
    target = [
      azurerm_role_assignment.workload_admin_password_secret_writer,
      azurerm_role_assignment.workload_kv_secrets_user,
      kubernetes_deployment.app,
    ]
  }

  variables {
    deploy_kubernetes_resources = true
  }

  assert {
    condition = anytrue([
      for e in kubernetes_deployment.app[0].spec[0].template[0].spec[0].container[0].env :
      e.name == "ADMIN_PASSWORD_SECRET" && e.value == "admin-password"
    ])
    error_message = "The granted secret must be the one passed to the pods as ADMIN_PASSWORD_SECRET."
  }

  assert {
    condition     = azurerm_role_assignment.workload_admin_password_secret_writer[0].scope == "${var.key_vault_id}/secrets/admin-password"
    error_message = "The single-secret scope must be known at plan time (scope is ForceNew)."
  }
}

run "no_admin_email_means_no_write_grant" {
  command = plan

  plan_options {
    target = [
      azurerm_role_assignment.workload_admin_password_secret_writer,
      azurerm_role_assignment.workload_kv_secrets_user,
    ]
  }

  variables {
    admin_email = ""
  }

  assert {
    condition     = length(azurerm_role_assignment.workload_admin_password_secret_writer) == 0
    error_message = "Without ADMIN_EMAIL the sync callback never arms, so no write grant may exist."
  }
}

run "no_secret_name_means_no_write_grant" {
  command = plan

  plan_options {
    target = [
      azurerm_role_assignment.workload_admin_password_secret_writer,
      azurerm_role_assignment.workload_kv_secrets_user,
    ]
  }

  variables {
    admin_password_secret_name = ""
  }

  assert {
    condition     = length(azurerm_role_assignment.workload_admin_password_secret_writer) == 0
    error_message = "Without ADMIN_PASSWORD_SECRET the sync callback never arms, so no write grant may exist."
  }
}

# A path in the name would move the grant's scope off the single secret.
run "secret_name_with_a_path_is_rejected" {
  command = plan

  plan_options {
    target = [
      azurerm_role_assignment.workload_admin_password_secret_writer,
      azurerm_role_assignment.workload_kv_secrets_user,
    ]
  }

  variables {
    admin_password_secret_name = "admin-password/../../secrets/other"
  }

  expect_failures = [var.admin_password_secret_name]
}
