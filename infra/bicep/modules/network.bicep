// Instance virtual network (HA-P3-21). AKS nodes and the PostgreSQL flexible
// server share it, so the database is reachable only over private addresses:
// the server is VNet-integrated through a delegated subnet and resolves via a
// private DNS zone linked here.
@description('Azure region for the network.')
param location string

@description('Virtual network name.')
param name string

@description('PostgreSQL flexible server name; names the private DNS zone.')
param postgresServerName string

@description('Address space of the virtual network.')
param addressPrefix string = '10.40.0.0/16'

@description('AKS node subnet.')
param aksSubnetPrefix string = '10.40.0.0/20'

@description('Subnet delegated to PostgreSQL flexible server.')
param postgresSubnetPrefix string = '10.40.16.0/24'

@description('Tags applied to network resources.')
param tags object = {}

resource vnet 'Microsoft.Network/virtualNetworks@2024-01-01' = {
  name: name
  location: location
  tags: tags
  properties: {
    addressSpace: {
      addressPrefixes: [
        addressPrefix
      ]
    }
    subnets: [
      {
        name: 'aks'
        properties: {
          addressPrefix: aksSubnetPrefix
        }
      }
      {
        name: 'postgres'
        properties: {
          addressPrefix: postgresSubnetPrefix
          delegations: [
            {
              name: 'postgres-flexible-server'
              properties: {
                serviceName: 'Microsoft.DBforPostgreSQL/flexibleServers'
              }
            }
          ]
        }
      }
    ]
  }
}

resource postgresDns 'Microsoft.Network/privateDnsZones@2024-06-01' = {
  name: '${postgresServerName}.private.postgres.database.azure.com'
  location: 'global'
  tags: tags
}

resource postgresDnsLink 'Microsoft.Network/privateDnsZones/virtualNetworkLinks@2024-06-01' = {
  parent: postgresDns
  name: '${name}-link'
  location: 'global'
  tags: tags
  properties: {
    registrationEnabled: false
    virtualNetwork: {
      id: vnet.id
    }
  }
}

@description('Resource ID of the AKS node subnet.')
output aksSubnetId string = resourceId('Microsoft.Network/virtualNetworks/subnets', vnet.name, 'aks')

@description('Resource ID of the subnet delegated to PostgreSQL.')
output postgresSubnetId string = resourceId('Microsoft.Network/virtualNetworks/subnets', vnet.name, 'postgres')

@description('Resource ID of the PostgreSQL private DNS zone (linked to the VNet).')
output postgresPrivateDnsZoneId string = postgresDns.id
