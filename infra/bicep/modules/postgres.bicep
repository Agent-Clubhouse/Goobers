// Azure Database for PostgreSQL flexible server: the goobers-cloud datastore
// and Temporal persistence (HA-P3-21, design D-6). General Purpose with
// zone-redundant HA (synchronous standby in another zone, RPO 0), private
// VNet access only, and Entra authentication. One server hosts
// goobers_<instance>, temporal and temporal_visibility; separate roles and no
// cross-database grants isolate them (internal/cloud/pgroles).
//
// Roles are not created here: Entra principals for the goobers roles and the
// temporal_app password role are created by `goobers-cloud bootstrap`
// (pgroles.Provision) connected as the Entra administrator below.
@description('Azure region for the server.')
param location string

@description('PostgreSQL flexible server name (globally unique, lowercase).')
param name string

@description('Goobers instance name; the instance database is goobers_<instance> with dashes as underscores.')
@minLength(1)
@maxLength(40)
param instanceName string

@description('Entra administrator used by bootstrap to create roles. principalType is User, Group or ServicePrincipal.')
param entraAdministrator object

@description('Entra tenant of the administrator and application identities.')
param tenantId string = subscription().tenantId

@description('Break-glass password administrator login.')
param administratorLogin string = 'gooberadmin'

@description('Break-glass administrator password. Pass via Key Vault reference; never hard-code (SEC-010).')
@secure()
param administratorPassword string

@description('Resource ID of the subnet delegated to PostgreSQL flexible server.')
param delegatedSubnetId string

@description('Resource ID of the linked private DNS zone (<name>.private.postgres.database.azure.com).')
param privateDnsZoneId string

@description('Compute SKU. Zone-redundant HA requires General Purpose or Memory Optimized.')
param skuName string = 'Standard_D2ds_v5'

@allowed([
  'GeneralPurpose'
  'MemoryOptimized'
])
param skuTier string = 'GeneralPurpose'

@description('Availability zones of the primary and the synchronous standby.')
param primaryZone string = '1'
param standbyZone string = '2'

@description('Storage size in GB; grows automatically.')
param storageSizeGB int = 128

@description('Point-in-time restore window in days.')
@minValue(7)
@maxValue(35)
param backupRetentionDays int = 35

@description('Geo-redundant backup storage. Cannot be changed after creation.')
@allowed([
  'Enabled'
  'Disabled'
])
param geoRedundantBackup string = 'Disabled'

@description('PostgreSQL major version. Role provisioning uses PostgreSQL 16 GRANT ... WITH SET syntax.')
@allowed([
  '16'
  '17'
])
param postgresVersion string = '16'

@description('Tags applied to the server.')
param tags object = {}

var instanceDatabase = 'goobers_${replace(toLower(instanceName), '-', '_')}'

resource server 'Microsoft.DBforPostgreSQL/flexibleServers@2024-08-01' = {
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
    // Goobers roles authenticate with Entra workload identity; Temporal has
    // no Entra token support, so temporal_app keeps a password.
    authConfig: {
      activeDirectoryAuth: 'Enabled'
      passwordAuth: 'Enabled'
      tenantId: tenantId
    }
    availabilityZone: primaryZone
    highAvailability: {
      mode: 'ZoneRedundant'
      standbyAvailabilityZone: standbyZone
    }
    storage: {
      storageSizeGB: storageSizeGB
      autoGrow: 'Enabled'
    }
    backup: {
      backupRetentionDays: backupRetentionDays
      geoRedundantBackup: geoRedundantBackup
    }
    network: {
      delegatedSubnetResourceId: delegatedSubnetId
      privateDnsZoneArmResourceId: privateDnsZoneId
      publicNetworkAccess: 'Disabled'
    }
  }
}

resource administrator 'Microsoft.DBforPostgreSQL/flexibleServers/administrators@2024-08-01' = {
  parent: server
  name: entraAdministrator.objectId
  properties: {
    principalName: entraAdministrator.principalName
    principalType: entraAdministrator.principalType
    tenantId: tenantId
  }
}

// App traffic uses the built-in PgBouncer on 6432 (transaction mode); the
// notifier and migrate use 5432 directly.
resource pgbouncer 'Microsoft.DBforPostgreSQL/flexibleServers/configurations@2024-08-01' = {
  parent: server
  name: 'pgbouncer.enabled'
  properties: {
    value: 'true'
    source: 'user-override'
  }
  dependsOn: [
    administrator
  ]
}

resource databases 'Microsoft.DBforPostgreSQL/flexibleServers/databases@2024-08-01' = [
  for database in [
    instanceDatabase
    'temporal'
    'temporal_visibility'
  ]: {
    parent: server
    name: database
    properties: {
      charset: 'UTF8'
      collation: 'en_US.utf8'
    }
    dependsOn: [
      pgbouncer
    ]
  }
]

@description('Resource ID of the PostgreSQL flexible server.')
output serverId string = server.id

@description('Private FQDN of the server.')
output fqdn string = server.properties.fullyQualifiedDomainName

@description('Instance database name.')
output instanceDatabase string = instanceDatabase

@description('Break-glass administrator login name.')
output administratorLogin string = administratorLogin
