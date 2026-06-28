// Azure Database for PostgreSQL (Flexible Server) — persistence for self-hosted
// Temporal (DEP-011). Temporal runs in-cluster; this is its managed datastore.
// The admin password is supplied as a Key Vault reference at deploy time (SEC-010).
@description('Azure region for the server.')
param location string

@description('PostgreSQL flexible server name (globally unique, lowercase).')
param name string

@description('Administrator login name.')
param administratorLogin string = 'gooberadmin'

@description('Administrator password. Pass via Key Vault reference; never hard-code (SEC-010).')
@secure()
param administratorPassword string

@description('Compute SKU. Defaults to a burstable dev tier.')
param skuName string = 'Standard_B1ms'
param skuTier string = 'Burstable'

@description('Storage size in GB.')
param storageSizeGB int = 32

@description('PostgreSQL major version.')
param postgresVersion string = '16'

@description('Tags applied to the server.')
param tags object = {}

resource server 'Microsoft.DBforPostgreSQL/flexibleServers@2023-06-01-preview' = {
  name: name
  location: location
  tags: tags
  sku: {
    name: skuName
    tier: skuTier
  }
  properties: {
    version: postgresVersion
    administratorLogin: administratorLogin
    administratorLoginPassword: administratorPassword
    storage: {
      storageSizeGB: storageSizeGB
    }
    backup: {
      backupRetentionDays: 7
      geoRedundantBackup: 'Disabled'
    }
    highAvailability: {
      mode: 'Disabled'
    }
    network: {
      publicNetworkAccess: 'Enabled'
    }
  }
}

// Temporal expects its databases to exist; it manages schema via temporal-sql-tool.
resource temporalDb 'Microsoft.DBforPostgreSQL/flexibleServers/databases@2023-06-01-preview' = {
  parent: server
  name: 'temporal'
  properties: {
    charset: 'UTF8'
    collation: 'en_US.utf8'
  }
}

resource temporalVisibilityDb 'Microsoft.DBforPostgreSQL/flexibleServers/databases@2023-06-01-preview' = {
  parent: server
  name: 'temporal_visibility'
  properties: {
    charset: 'UTF8'
    collation: 'en_US.utf8'
  }
}

// Allow other Azure services (incl. AKS egress via Azure backbone) to reach the server.
// Tighten to private networking / VNet integration for production (build-time hardening).
resource allowAzure 'Microsoft.DBforPostgreSQL/flexibleServers/firewallRules@2023-06-01-preview' = {
  parent: server
  name: 'AllowAllAzureServices'
  properties: {
    startIpAddress: '0.0.0.0'
    endIpAddress: '0.0.0.0'
  }
}

@description('Resource ID of the PostgreSQL flexible server.')
output serverId string = server.id

@description('Fully-qualified domain name Temporal connects to.')
output fqdn string = server.properties.fullyQualifiedDomainName

@description('Administrator login name.')
output administratorLogin string = administratorLogin
