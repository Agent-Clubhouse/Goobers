// Log Analytics workspace — instance logging + AKS Container Insights sink (DEP-001, INST-003).
@description('Azure region for the workspace.')
param location string

@description('Workspace name.')
param name string

@description('Retention in days for collected logs.')
@minValue(30)
@maxValue(730)
param retentionInDays int = 30

@description('Tags applied to the workspace.')
param tags object = {}

resource workspace 'Microsoft.OperationalInsights/workspaces@2022-10-01' = {
  name: name
  location: location
  tags: tags
  properties: {
    sku: {
      name: 'PerGB2018'
    }
    retentionInDays: retentionInDays
    features: {
      enableLogAccessUsingOnlyResourcePermissions: true
    }
  }
}

@description('Resource ID of the Log Analytics workspace.')
output workspaceId string = workspace.id

@description('Customer ID (workspace GUID) used by agents.')
output customerId string = workspace.properties.customerId
