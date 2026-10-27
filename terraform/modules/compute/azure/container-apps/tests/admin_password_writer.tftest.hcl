# The admin-password sync writes the new password back to Key Vault
# (azsecrets.SetSecret, internal/server/app.go). The vault-wide grant the
# Container App identity holds is the read-only "Key Vault Secrets User", so
# the identity needs a write grant on that one secret and nowhere else (#199).
mock_provider "azurerm" {
  # Resources referenced by ID elsewhere in the module are parsed, so the mock
  # needs well-formed ARM IDs.
  mock_resource "azurerm_user_assigned_identity" {
    defaults = {
      id           = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/cudly-test-rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/cudly-test-identity"
      principal_id = "22222222-2222-2222-2222-222222222222"
      client_id    = "33333333-3333-3333-3333-333333333333"
    }
  }
  mock_resource "azurerm_container_app_environment" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/cudly-test-rg/providers/Microsoft.App/managedEnvironments/cudly-test-env"
    }
  }
}

variables {
  app_name                       = "cudly-test"
  environment                    = "dev"
  resource_group_name            = "cudly-test-rg"
  location                       = "eastus2"
  image_uri                      = "cudlytest.azurecr.io/cudly:test"
  database_host                  = "cudly-test-postgres.postgres.database.azure.com"
  database_name                  = "cudly"
  database_username              = "cudlyadmin"
  database_password_secret_name  = "database-password"
  subscription_id                = "00000000-0000-0000-0000-000000000000"
  reservation_role_definition_id = "/subscriptions/00000000-0000-0000-0000-000000000000/providers/Microsoft.Authorization/roleDefinitions/11111111-1111-1111-1111-111111111111"
  key_vault_uri                  = "https://cudly-test-kv.vault.azure.net/"
  key_vault_id                   = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/cudly-test-rg/providers/Microsoft.KeyVault/vaults/cudly-test-kv"
  signing_key_name               = "oidc-signing-key"
  registry_server                = "cudlytest.azurecr.io"
  container_registry_id          = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/cudly-test-rg/providers/Microsoft.ContainerRegistry/registries/cudlytest"
  admin_email                    = "admin@example.com"
  admin_password_secret_name     = "admin-password"
  enable_scheduled_tasks         = false
  infrastructure_subnet_id       = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/cudly-test-rg/providers/Microsoft.Network/virtualNetworks/vnet/subnets/apps"
  internal_load_balancer_enabled = false
}

run "sync_armed_grants_write_on_that_secret_only" {
  command = apply

  assert {
    condition     = length(azurerm_role_assignment.admin_password_secret_writer) == 1
    error_message = "With ADMIN_PASSWORD_SECRET and ADMIN_EMAIL both set the sync is armed, so the identity needs a write grant or SetSecret 403s."
  }

  assert {
    condition     = azurerm_role_assignment.admin_password_secret_writer[0].scope == "${var.key_vault_id}/secrets/admin-password"
    error_message = "The write grant must be scoped to the admin-password secret itself, not the vault or anything wider."
  }

  assert {
    condition     = azurerm_role_assignment.admin_password_secret_writer[0].role_definition_name == "Key Vault Secrets Officer"
    error_message = "Key Vault Secrets Officer is the built-in role that carries secrets/setSecret/action."
  }

  assert {
    condition     = azurerm_role_assignment.admin_password_secret_writer[0].principal_id == azurerm_user_assigned_identity.container_app.principal_id
    error_message = "The write grant must go to the Container App's own identity, the one the app authenticates as (AZURE_CLIENT_ID)."
  }

  # The secret named in the grant must be the one the app writes.
  assert {
    condition = anytrue([
      for e in azurerm_container_app.main.template[0].container[0].env :
      e.name == "ADMIN_PASSWORD_SECRET" && e.value == "admin-password"
    ])
    error_message = "The granted secret must be the one passed to the app as ADMIN_PASSWORD_SECRET."
  }

  # Nothing broader: no other grant in this module carries a secrets write role.
  assert {
    condition = alltrue([
      azurerm_role_assignment.signing_key_crypto_user.role_definition_name == "Key Vault Crypto User",
      azurerm_role_assignment.acr_pull.role_definition_name == "AcrPull",
      azurerm_role_assignment.cost_management_reader.role_definition_name == "Cost Management Reader",
      azurerm_role_assignment.subscription_reader.role_definition_name == "Reader",
    ])
    error_message = "Only the per-secret grant may carry Key Vault Secrets Officer."
  }
}

run "no_admin_email_means_no_write_grant" {
  command = plan

  variables {
    admin_email = ""
  }

  assert {
    condition     = length(azurerm_role_assignment.admin_password_secret_writer) == 0
    error_message = "Without ADMIN_EMAIL the sync callback never arms, so no write grant may exist."
  }
}

run "no_secret_name_means_no_write_grant" {
  command = plan

  variables {
    admin_password_secret_name = ""
  }

  assert {
    condition     = length(azurerm_role_assignment.admin_password_secret_writer) == 0
    error_message = "Without ADMIN_PASSWORD_SECRET the sync callback never arms, so no write grant may exist."
  }
}

# A path in the name would move the grant's scope off the single secret.
run "secret_name_with_a_path_is_rejected" {
  command = plan

  variables {
    admin_password_secret_name = "admin-password/../../secrets/other"
  }

  expect_failures = [var.admin_password_secret_name]
}
