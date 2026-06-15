targetScope = 'resourceGroup'

@description('Azure region for all resources.')
param location string = resourceGroup().location

@description('Prefix used for resource names. Keep it short enough for globally unique Key Vault naming.')
param namePrefix string = 'ghcp-pool'

@description('Container image to run, for example ghcr.io/owner/ghcp-pool-go:latest or an ACR image.')
param containerImage string

@secure()
@description('Gateway API key stored as the Key Vault secret used by GHCP_API_KEY_KEY_VAULT_SECRET.')
param gatewayApiKey string

@secure()
@description('GitHub/Copilot OAuth token stored as the Key Vault secret used by GHCP_COPILOT_TOKEN_KEY_VAULT_SECRET.')
param copilotToken string

@description('Key Vault secret name for the gateway API key.')
param gatewayApiSecretName string = 'ghcp-api-key'

@description('Key Vault secret name for the default Copilot token.')
param copilotTokenSecretName string = 'ghcp-copilot-token'

@description('Address space for the private VNet.')
param vnetAddressPrefix string = '10.42.0.0/16'

@description('Subnet delegated to Microsoft.App/environments. A /23 or larger is recommended for Container Apps.')
param containerAppsSubnetPrefix string = '10.42.0.0/23'

@description('Subnet used by the Key Vault private endpoint.')
param privateEndpointSubnetPrefix string = '10.42.2.0/24'

@description('When true, the Container Apps environment uses an internal load balancer and has no public environment endpoint.')
param internalEnvironment bool = true

@description('When true, the app ingress is reachable from outside the Container Apps environment. In an internal environment this is VNet-private, not public internet.')
param appIngressExternal bool = true

@description('HTTP port exposed by ghcp-pool-go.')
param appPort int = 8000

@description('Minimum Container App replicas.')
param minReplicas int = 1

@description('Maximum Container App replicas.')
param maxReplicas int = 3

@allowed([
  '0.25'
  '0.5'
  '0.75'
  '1.0'
  '1.25'
  '1.5'
  '1.75'
  '2.0'
])
@description('CPU cores assigned to the container.')
param containerCpu string = '0.5'

@allowed([
  '0.5Gi'
  '1.0Gi'
  '1.5Gi'
  '2.0Gi'
  '3.0Gi'
  '4.0Gi'
])
@description('Memory assigned to the container.')
param containerMemory string = '1.0Gi'

@description('Optional tags applied to all resources.')
param tags object = {}

var safePrefix = take(toLower(replace(namePrefix, '-', '')), 12)
var suffix = uniqueString(resourceGroup().id, namePrefix)
var keyVaultName = take('${safePrefix}kv${suffix}', 24)
var vnetName = '${namePrefix}-vnet'
var containerAppsSubnetName = 'container-apps'
var privateEndpointSubnetName = 'private-endpoints'
var logAnalyticsName = '${namePrefix}-logs'
var managedEnvironmentName = '${namePrefix}-env'
var appIdentityName = '${namePrefix}-mi'
var appName = '${namePrefix}-app'
var privateEndpointName = '${namePrefix}-kv-pe'
var privateDnsZoneName = 'privatelink.vaultcore.azure.net'
var resourceTags = union(tags, {
  workload: 'ghcp-pool-go'
})
var keyVaultSecretsUserRoleDefinitionId = subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '4633458b-17de-408a-b874-0445c86b69e6')

resource vnet 'Microsoft.Network/virtualNetworks@2023-11-01' = {
  name: vnetName
  location: location
  tags: resourceTags
  properties: {
    addressSpace: {
      addressPrefixes: [
        vnetAddressPrefix
      ]
    }
    subnets: [
      {
        name: containerAppsSubnetName
        properties: {
          addressPrefix: containerAppsSubnetPrefix
          delegations: [
            {
              name: 'container-apps-delegation'
              properties: {
                serviceName: 'Microsoft.App/environments'
              }
            }
          ]
        }
      }
      {
        name: privateEndpointSubnetName
        properties: {
          addressPrefix: privateEndpointSubnetPrefix
          privateEndpointNetworkPolicies: 'Disabled'
        }
      }
    ]
  }
}

resource containerAppsSubnet 'Microsoft.Network/virtualNetworks/subnets@2023-11-01' existing = {
  parent: vnet
  name: containerAppsSubnetName
}

resource privateEndpointSubnet 'Microsoft.Network/virtualNetworks/subnets@2023-11-01' existing = {
  parent: vnet
  name: privateEndpointSubnetName
}

resource privateDnsZone 'Microsoft.Network/privateDnsZones@2020-06-01' = {
  name: privateDnsZoneName
  location: 'global'
  tags: resourceTags
}

resource privateDnsLink 'Microsoft.Network/privateDnsZones/virtualNetworkLinks@2020-06-01' = {
  parent: privateDnsZone
  name: '${namePrefix}-vaultcore-link'
  location: 'global'
  tags: resourceTags
  properties: {
    registrationEnabled: false
    virtualNetwork: {
      id: vnet.id
    }
  }
}

resource logAnalytics 'Microsoft.OperationalInsights/workspaces@2022-10-01' = {
  name: logAnalyticsName
  location: location
  tags: resourceTags
  properties: {
    sku: {
      name: 'PerGB2018'
    }
    retentionInDays: 30
  }
}

resource keyVault 'Microsoft.KeyVault/vaults@2023-07-01' = {
  name: keyVaultName
  location: location
  tags: resourceTags
  properties: {
    tenantId: tenant().tenantId
    sku: {
      family: 'A'
      name: 'standard'
    }
    enableRbacAuthorization: true
    enablePurgeProtection: true
    softDeleteRetentionInDays: 7
    publicNetworkAccess: 'Disabled'
    networkAcls: {
      bypass: 'AzureServices'
      defaultAction: 'Deny'
    }
  }
}

resource gatewayApiSecret 'Microsoft.KeyVault/vaults/secrets@2023-07-01' = {
  parent: keyVault
  name: gatewayApiSecretName
  properties: {
    value: gatewayApiKey
    contentType: 'ghcp-pool-go gateway API key'
  }
}

resource copilotTokenSecret 'Microsoft.KeyVault/vaults/secrets@2023-07-01' = {
  parent: keyVault
  name: copilotTokenSecretName
  properties: {
    value: copilotToken
    contentType: 'ghcp-pool-go Copilot OAuth token'
  }
}

resource keyVaultPrivateEndpoint 'Microsoft.Network/privateEndpoints@2023-11-01' = {
  name: privateEndpointName
  location: location
  tags: resourceTags
  properties: {
    subnet: {
      id: privateEndpointSubnet.id
    }
    privateLinkServiceConnections: [
      {
        name: 'keyvault'
        properties: {
          privateLinkServiceId: keyVault.id
          groupIds: [
            'vault'
          ]
        }
      }
    ]
  }
}

resource keyVaultPrivateDnsZoneGroup 'Microsoft.Network/privateEndpoints/privateDnsZoneGroups@2023-11-01' = {
  parent: keyVaultPrivateEndpoint
  name: 'default'
  properties: {
    privateDnsZoneConfigs: [
      {
        name: 'vaultcore'
        properties: {
          privateDnsZoneId: privateDnsZone.id
        }
      }
    ]
  }
  dependsOn: [
    privateDnsLink
  ]
}

resource appIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: appIdentityName
  location: location
  tags: resourceTags
}

resource keyVaultSecretsUser 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(keyVault.id, appIdentity.id, 'Key Vault Secrets User')
  scope: keyVault
  properties: {
    roleDefinitionId: keyVaultSecretsUserRoleDefinitionId
    principalId: appIdentity.properties.principalId
    principalType: 'ServicePrincipal'
  }
}

resource managedEnvironment 'Microsoft.App/managedEnvironments@2024-03-01' = {
  name: managedEnvironmentName
  location: location
  tags: resourceTags
  properties: {
    appLogsConfiguration: {
      destination: 'log-analytics'
      logAnalyticsConfiguration: {
        customerId: logAnalytics.properties.customerId
        sharedKey: logAnalytics.listKeys().primarySharedKey
      }
    }
    vnetConfiguration: {
      infrastructureSubnetId: containerAppsSubnet.id
      internal: internalEnvironment
    }
  }
}

resource containerApp 'Microsoft.App/containerApps@2024-03-01' = {
  name: appName
  location: location
  tags: resourceTags
  identity: {
    type: 'UserAssigned'
    userAssignedIdentities: {
      '${appIdentity.id}': {}
    }
  }
  properties: {
    managedEnvironmentId: managedEnvironment.id
    configuration: {
      activeRevisionsMode: 'Single'
      ingress: {
        external: appIngressExternal
        targetPort: appPort
        transport: 'auto'
        traffic: [
          {
            latestRevision: true
            weight: 100
          }
        ]
      }
    }
    template: {
      containers: [
        {
          name: 'ghcp-pool'
          image: containerImage
          env: [
            {
              name: 'GHCP_BACKEND'
              value: 'copilot'
            }
            {
              name: 'GHCP_HOST'
              value: '0.0.0.0'
            }
            {
              name: 'GHCP_PORT'
              value: string(appPort)
            }
            {
              name: 'AZURE_CLIENT_ID'
              value: appIdentity.properties.clientId
            }
            {
              name: 'AZURE_KEY_VAULT_URL'
              value: keyVault.properties.vaultUri
            }
            {
              name: 'GHCP_API_KEY_KEY_VAULT_SECRET'
              value: gatewayApiSecretName
            }
            {
              name: 'GHCP_COPILOT_TOKEN_KEY_VAULT_SECRET'
              value: copilotTokenSecretName
            }
          ]
          resources: {
            cpu: json(containerCpu)
            memory: containerMemory
          }
        }
      ]
      scale: {
        minReplicas: minReplicas
        maxReplicas: maxReplicas
      }
    }
  }
  dependsOn: [
    gatewayApiSecret
    copilotTokenSecret
    keyVaultPrivateDnsZoneGroup
    keyVaultSecretsUser
  ]
}

output containerAppName string = containerApp.name
output containerAppFqdn string = containerApp.properties.configuration.ingress.fqdn
output keyVaultName string = keyVault.name
output keyVaultUri string = keyVault.properties.vaultUri
output userAssignedIdentityClientId string = appIdentity.properties.clientId
output virtualNetworkName string = vnet.name
