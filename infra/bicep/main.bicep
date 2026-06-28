// Goobers instance bootstrap (DEP-001, DEP-002, INST-001/003).
//
// Stage 1 of the two-stage GitOps deploy: a release pipeline applies this template to
// provision the shared instance infra — AKS, Log Analytics, storage, Key Vault, the
// ADX goober-run telemetry store, and the PostgreSQL datastore backing in-cluster
// Temporal. Stage 2 (ArgoCD) then reconciles the separate `config` repo into the
// cluster. This file provisions infra only; it does not deploy the workforce.
targetScope = 'subscription'

@description('Short prefix for all resource names (lowercase letters/digits).')
@minLength(3)
@maxLength(12)
param namePrefix string = 'goobers'

@description('Environment discriminator (dev/test/prod). Instances are separate per env (INST-Q1).')
@allowed([
  'dev'
  'test'
  'prod'
])
param environment string = 'dev'

@description('Azure region for all resources.')
param location string = 'eastus'

@description('Entra (AAD) tenant ID. Defaults to the deployment subscription tenant.')
param tenantId string = subscription().tenantId

@description('PostgreSQL administrator password for Temporal persistence. Supply via Key Vault reference (SEC-010).')
@secure()
param postgresAdminPassword string

@description('Tags applied to every resource.')
param tags object = {
  product: 'goobers'
  environment: environment
  managedBy: 'infra-repo'
}

var suffix = take(uniqueString(subscription().id, namePrefix, environment), 13)
var rgName = 'rg-${namePrefix}-${environment}'

resource rg 'Microsoft.Resources/resourceGroups@2024-03-01' = {
  name: rgName
  location: location
  tags: tags
}

module logs 'modules/loganalytics.bicep' = {
  scope: rg
  name: 'loganalytics'
  params: {
    location: location
    name: 'log-${namePrefix}-${environment}'
    tags: tags
  }
}

module storage 'modules/storage.bicep' = {
  scope: rg
  name: 'storage'
  params: {
    location: location
    name: take(toLower('${namePrefix}st${suffix}'), 24)
    tags: tags
  }
}

module keyvault 'modules/keyvault.bicep' = {
  scope: rg
  name: 'keyvault'
  params: {
    location: location
    name: take('kv-${namePrefix}-${suffix}', 24)
    tenantId: tenantId
    tags: tags
  }
}

module adx 'modules/adx.bicep' = {
  scope: rg
  name: 'adx'
  params: {
    location: location
    clusterName: take(toLower('${namePrefix}adx${suffix}'), 22)
    tags: tags
  }
}

module postgres 'modules/postgres.bicep' = {
  scope: rg
  name: 'postgres'
  params: {
    location: location
    name: 'psql-${namePrefix}-${suffix}'
    administratorPassword: postgresAdminPassword
    tags: tags
  }
}

module aks 'modules/aks.bicep' = {
  scope: rg
  name: 'aks'
  params: {
    location: location
    name: 'aks-${namePrefix}-${environment}'
    logAnalyticsWorkspaceId: logs.outputs.workspaceId
    tags: tags
  }
}

@description('Resource group hosting the instance.')
output resourceGroupName string = rg.name

@description('AKS cluster name (for `az aks get-credentials`).')
output aksClusterName string = aks.outputs.clusterName

@description('OIDC issuer URL for federating per-gaggle workload identities (SEC-002).')
output aksOidcIssuerUrl string = aks.outputs.oidcIssuerUrl

@description('ADX telemetry ingestion URI (OTel exporter target, TEL-010).')
output adxClusterUri string = adx.outputs.clusterUri

@description('PostgreSQL FQDN backing Temporal (DEP-011).')
output postgresFqdn string = postgres.outputs.fqdn

@description('Key Vault URI consumed by the CSI driver (SEC-Q2).')
output keyVaultUri string = keyvault.outputs.keyVaultUri
