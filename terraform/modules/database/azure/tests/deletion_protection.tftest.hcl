# The Azure database module must match the AWS and GCP modules' deletion
# posture (#293): protected unless the caller opts out. Flexible Server has
# no API-side flag, so protection is a CanNotDelete management lock on the
# server.
mock_provider "azurerm" {
  # The child resources parse server_id, so the mock needs a well-formed ID.
  mock_resource "azurerm_postgresql_flexible_server" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/cudly-test-rg/providers/Microsoft.DBforPostgreSQL/flexibleServers/cudly-test-postgres"
    }
  }
}

variables {
  app_name               = "cudly-test"
  environment            = "prod"
  resource_group_name    = "cudly-test-rg"
  location               = "eastus2"
  administrator_password = "mock"
  delegated_subnet_id    = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/cudly-test-rg/providers/Microsoft.Network/virtualNetworks/vnet/subnets/db"
  private_dns_zone_id    = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/cudly-test-rg/providers/Microsoft.Network/privateDnsZones/cudly.postgres.database.azure.com"
}

run "locked_by_default" {
  command = apply

  assert {
    condition     = length(azurerm_management_lock.server) == 1
    error_message = "A caller that omits deletion_protection must get a delete lock on the server, as the AWS module defaults to protected."
  }

  assert {
    condition     = azurerm_management_lock.server[0].lock_level == "CanNotDelete"
    error_message = "The server lock must be CanNotDelete; ReadOnly would block routine updates."
  }

  assert {
    condition     = azurerm_management_lock.server[0].scope == azurerm_postgresql_flexible_server.main.id
    error_message = "The lock must be scoped to the PostgreSQL Flexible Server itself."
  }
}

run "opt_out_has_no_lock" {
  command = plan

  variables {
    environment         = "dev"
    deletion_protection = false
  }

  assert {
    condition     = length(azurerm_management_lock.server) == 0
    error_message = "deletion_protection = false must leave the server unlocked so ephemeral environments can be destroyed."
  }
}
