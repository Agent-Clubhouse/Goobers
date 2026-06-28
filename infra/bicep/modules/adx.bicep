// Azure Data Explorer (Kusto) — the goober-run telemetry store (TEL-001, DEP-001).
// Separate from any project telemetry; OTel traces/spans are exported here (TEL-010).
// Per-gaggle partitioning is enforced at the table/policy level, not here (TEL-Q4/SEC-003).
@description('Azure region for the ADX cluster.')
param location string

@description('ADX cluster name (4-22 lowercase alphanumerics, globally unique).')
@minLength(4)
@maxLength(22)
param clusterName string

@description('Telemetry database name.')
param databaseName string = 'gooberrun'

@description('Hot-cache / retention window for the telemetry database (configurable per instance, TEL-Q1).')
param softDeletePeriod string = 'P30D'
param hotCachePeriod string = 'P7D'

@description('SKU for the cluster. Dev/test defaults to a single low-tier node.')
param skuName string = 'Standard_E2ads_v5'
param skuTier string = 'Standard'
@minValue(1)
@maxValue(2)
param capacity int = 1

@description('Tags applied to the cluster.')
param tags object = {}

resource cluster 'Microsoft.Kusto/clusters@2023-08-15' = {
  name: clusterName
  location: location
  tags: tags
  sku: {
    name: skuName
    tier: skuTier
    capacity: capacity
  }
  identity: {
    type: 'SystemAssigned'
  }
  properties: {
    enableStreamingIngest: true
    enablePurge: true
    enableDiskEncryption: true
  }
}

resource database 'Microsoft.Kusto/clusters/databases@2023-08-15' = {
  parent: cluster
  name: databaseName
  location: location
  kind: 'ReadWrite'
  properties: {
    softDeletePeriod: softDeletePeriod
    hotCachePeriod: hotCachePeriod
  }
}

@description('Resource ID of the ADX cluster.')
output clusterId string = cluster.id

@description('Cluster data-ingestion URI (OTel exporter target).')
output clusterUri string = cluster.properties.uri

@description('Telemetry database name.')
output databaseName string = database.name
