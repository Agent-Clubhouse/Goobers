// Storage account — instance artifact/state storage + disk (DEP-001, INST-003).
@description('Azure region for the storage account.')
param location string

@description('Globally-unique storage account name (3-24 lowercase alphanumerics).')
@minLength(3)
@maxLength(24)
param name string

@description('Tags applied to the storage account.')
param tags object = {}

resource storage 'Microsoft.Storage/storageAccounts@2023-01-01' = {
  name: name
  location: location
  tags: tags
  sku: {
    name: 'Standard_LRS'
  }
  kind: 'StorageV2'
  properties: {
    accessTier: 'Hot'
    minimumTlsVersion: 'TLS1_2'
    supportsHttpsTrafficOnly: true
    allowBlobPublicAccess: false
    networkAcls: {
      defaultAction: 'Deny'
      bypass: 'AzureServices'
    }
  }
}

@description('Resource ID of the storage account.')
output storageAccountId string = storage.id

@description('Storage account name.')
output storageAccountName string = storage.name
