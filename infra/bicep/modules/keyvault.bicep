// Key Vault — secrets are *referenced* from here, never stored in the infra/config
// repos (SEC-010, CFG-009). Consumed in-cluster via the Key Vault CSI driver (SEC-Q2).
@description('Azure region for the vault.')
param location string

@description('Globally-unique Key Vault name (3-24 alphanumerics/hyphens).')
@minLength(3)
@maxLength(24)
param name string

@description('Entra (AAD) tenant ID the vault trusts.')
param tenantId string

@description('Tags applied to the vault.')
param tags object = {}

resource vault 'Microsoft.KeyVault/vaults@2023-07-01' = {
  name: name
  location: location
  tags: tags
  properties: {
    tenantId: tenantId
    sku: {
      family: 'A'
      name: 'standard'
    }
    // RBAC over access policies: per-gaggle workload identities are granted
    // scoped 'Key Vault Secrets User' roles out of band (least privilege, SEC-011).
    enableRbacAuthorization: true
    enableSoftDelete: true
    softDeleteRetentionInDays: 7
    enablePurgeProtection: true
    publicNetworkAccess: 'Disabled'
    networkAcls: {
      defaultAction: 'Deny'
      bypass: 'AzureServices'
    }
  }
}

@description('Resource ID of the Key Vault.')
output keyVaultId string = vault.id

@description('Key Vault name.')
output keyVaultName string = vault.name

@description('Key Vault URI used by the CSI driver.')
output keyVaultUri string = vault.properties.vaultUri
