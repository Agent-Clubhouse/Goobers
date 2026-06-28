// AKS cluster — hosts Temporal, the Goobers operator, and ephemeral run pods
// (DEP-001, DEP-004, INST-003). OIDC issuer + workload identity are enabled so each
// gaggle gets its own federated Azure identity (SEC-002, SEC-Q1). Container Insights
// ships logs to Log Analytics; the Key Vault CSI driver injects secrets (SEC-Q2).
@description('Azure region for the cluster.')
param location string

@description('AKS cluster name.')
param name string

@description('DNS prefix for the cluster API server.')
param dnsPrefix string = name

@description('Kubernetes version.')
param kubernetesVersion string = '1.30'

@description('System node pool VM size.')
param systemVmSize string = 'Standard_D2s_v5'

@description('System node pool node count.')
@minValue(1)
@maxValue(5)
param systemNodeCount int = 2

@description('Resource ID of the Log Analytics workspace for Container Insights.')
param logAnalyticsWorkspaceId string

@description('Tags applied to the cluster.')
param tags object = {}

resource aks 'Microsoft.ContainerService/managedClusters@2024-02-01' = {
  name: name
  location: location
  tags: tags
  identity: {
    type: 'SystemAssigned'
  }
  properties: {
    dnsPrefix: dnsPrefix
    kubernetesVersion: kubernetesVersion
    enableRBAC: true
    // OIDC issuer + workload identity: federate per-gaggle managed identities (SEC-002).
    oidcIssuerProfile: {
      enabled: true
    }
    securityProfile: {
      workloadIdentity: {
        enabled: true
      }
    }
    aadProfile: {
      managed: true
      enableAzureRBAC: true
    }
    agentPoolProfiles: [
      {
        name: 'system'
        mode: 'System'
        count: systemNodeCount
        vmSize: systemVmSize
        osType: 'Linux'
        osSKU: 'AzureLinux'
        type: 'VirtualMachineScaleSets'
        enableAutoScaling: true
        minCount: systemNodeCount
        maxCount: systemNodeCount + 3
      }
    ]
    networkProfile: {
      networkPlugin: 'azure'
      networkPluginMode: 'overlay'
      networkPolicy: 'cilium'
      networkDataplane: 'cilium'
    }
    addonProfiles: {
      omsagent: {
        enabled: true
        config: {
          logAnalyticsWorkspaceResourceID: logAnalyticsWorkspaceId
        }
      }
      azureKeyvaultSecretsProvider: {
        enabled: true
        config: {
          enableSecretRotation: 'true'
        }
      }
    }
  }
}

@description('Resource ID of the AKS cluster.')
output clusterId string = aks.id

@description('AKS cluster name.')
output clusterName string = aks.name

@description('OIDC issuer URL used to federate per-gaggle workload identities.')
output oidcIssuerUrl string = aks.properties.oidcIssuerProfile.issuerURL

@description('Object ID of the cluster kubelet identity (for ACR pulls etc.).')
output kubeletObjectId string = aks.properties.identityProfile.kubeletidentity.objectId
