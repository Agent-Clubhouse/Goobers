// Grants the AKS control-plane identity Network Contributor on its node subnet,
// which a cluster in a bring-your-own VNet needs to manage load balancers.
@description('Virtual network holding the AKS node subnet.')
param vnetName string

@description('AKS node subnet name.')
param subnetName string = 'aks'

@description('Principal ID of the AKS control-plane (system-assigned) identity.')
param aksPrincipalId string

var networkContributor = subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '4d97b98b-1d4f-4787-a291-c67834d212e7')

resource subnet 'Microsoft.Network/virtualNetworks/subnets@2024-01-01' existing = {
  name: '${vnetName}/${subnetName}'
}

resource access 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  scope: subnet
  name: guid(subnet.id, aksPrincipalId, networkContributor)
  properties: {
    roleDefinitionId: networkContributor
    principalId: aksPrincipalId
    principalType: 'ServicePrincipal'
  }
}
