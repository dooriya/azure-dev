@description('Name of the azd environment.')
param environmentName string

@description('Primary Azure location for all resources.')
param location string = resourceGroup().location

@description('Object ID of the user or service principal running azd.')
param principalId string

@description('Foundry catalog model name.')
param modelName string

@description('Foundry catalog model version.')
param modelVersion string

@description('Name of the model deployment.')
param modelDeploymentName string

@description('Model deployment SKU.')
param modelSku string

@description('Model deployment capacity.')
param modelCapacity int

@description('Tags applied to all resources.')
param tags object = {}

var resourceToken = toLower(uniqueString(subscription().id, resourceGroup().id, environmentName, location))
var accountName = 'aieval${take(resourceToken, 18)}'
var projectName = 'eval-${take(resourceToken, 16)}'
var appInsightsName = 'appi-eval-${take(resourceToken, 12)}'
var workspaceName = 'log-eval-${take(resourceToken, 12)}'

var foundryUserRoleId = subscriptionResourceId(
  'Microsoft.Authorization/roleDefinitions',
  '53ca6127-db72-4b80-b1b0-d745d6d5456d'
)
var logAnalyticsReaderRoleId = subscriptionResourceId(
  'Microsoft.Authorization/roleDefinitions',
  '73c42c96-874c-492b-b04d-ab87d138a893'
)
var privilegedMonitoringDataReaderRoleId = subscriptionResourceId(
  'Microsoft.Authorization/roleDefinitions',
  'dbc9c667-e97f-4491-aee6-90b9cf960190'
)

resource workspace 'Microsoft.OperationalInsights/workspaces@2023-09-01' = {
  name: workspaceName
  location: location
  tags: tags
  properties: {
    retentionInDays: 30
    sku: {
      name: 'PerGB2018'
    }
  }
}

resource applicationInsights 'Microsoft.Insights/components@2020-02-02' = {
  name: appInsightsName
  location: location
  kind: 'web'
  tags: tags
  properties: {
    Application_Type: 'web'
    IngestionMode: 'LogAnalytics'
    WorkspaceResourceId: workspace.id
    publicNetworkAccessForIngestion: 'Enabled'
    publicNetworkAccessForQuery: 'Enabled'
  }
}

resource foundryAccount 'Microsoft.CognitiveServices/accounts@2025-06-01' = {
  name: accountName
  location: location
  kind: 'AIServices'
  sku: {
    name: 'S0'
  }
  identity: {
    type: 'SystemAssigned'
  }
  tags: tags
  properties: {
    allowProjectManagement: true
    customSubDomainName: accountName
    disableLocalAuth: true
    publicNetworkAccess: 'Enabled'
    networkAcls: {
      defaultAction: 'Allow'
      virtualNetworkRules: []
      ipRules: []
    }
  }

  resource modelDeployment 'deployments' = {
    name: modelDeploymentName
    properties: {
      model: {
        format: 'OpenAI'
        name: modelName
        version: modelVersion
      }
      versionUpgradeOption: 'OnceNewDefaultVersionAvailable'
    }
    sku: {
      name: modelSku
      capacity: modelCapacity
    }
  }

  resource project 'projects' = {
    name: projectName
    location: location
    identity: {
      type: 'SystemAssigned'
    }
    properties: {
      description: 'Model evaluation project managed by azd.'
      displayName: projectName
    }
    dependsOn: [
      modelDeployment
    ]
  }
}

resource accountAppInsightsConnection 'Microsoft.CognitiveServices/accounts/connections@2025-04-01-preview' = {
  parent: foundryAccount
  name: 'appi-${take(resourceToken, 12)}'
  properties: {
    category: 'AppInsights'
    target: applicationInsights.id
    authType: 'ApiKey'
    isSharedToAll: true
    credentials: {
      key: applicationInsights.properties.ConnectionString
    }
    metadata: {
      ApiType: 'Azure'
      ResourceId: applicationInsights.id
    }
  }
}

resource projectAppInsightsConnection 'Microsoft.CognitiveServices/accounts/projects/connections@2025-04-01-preview' = {
  parent: foundryAccount::project
  name: appInsightsName
  properties: {
    category: 'AppInsights'
    target: applicationInsights.id
    authType: 'ApiKey'
    isSharedToAll: true
    credentials: {
      key: applicationInsights.properties.ConnectionString
    }
    metadata: {
      ApiType: 'Azure'
      ResourceId: applicationInsights.id
    }
  }
}

resource foundryUserRole 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (!empty(principalId)) {
  name: guid(foundryAccount::project.id, principalId, foundryUserRoleId)
  scope: foundryAccount::project
  properties: {
    principalId: principalId
    roleDefinitionId: foundryUserRoleId
  }
}

resource developerAppInsightsReader 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (!empty(principalId)) {
  name: guid(applicationInsights.id, principalId, logAnalyticsReaderRoleId)
  scope: applicationInsights
  properties: {
    principalId: principalId
    roleDefinitionId: logAnalyticsReaderRoleId
  }
}

resource developerWorkspaceReader 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (!empty(principalId)) {
  name: guid(workspace.id, principalId, logAnalyticsReaderRoleId)
  scope: workspace
  properties: {
    principalId: principalId
    roleDefinitionId: logAnalyticsReaderRoleId
  }
}

resource projectAppInsightsReaders 'Microsoft.Authorization/roleAssignments@2022-04-01' = [
  for roleId in [
    logAnalyticsReaderRoleId
    privilegedMonitoringDataReaderRoleId
  ]: {
    name: guid(applicationInsights.id, foundryAccount::project.id, roleId)
    scope: applicationInsights
    properties: {
      principalId: foundryAccount::project.identity.principalId
      principalType: 'ServicePrincipal'
      roleDefinitionId: roleId
    }
  }
]

resource projectWorkspaceReaders 'Microsoft.Authorization/roleAssignments@2022-04-01' = [
  for roleId in [
    logAnalyticsReaderRoleId
    privilegedMonitoringDataReaderRoleId
  ]: {
    name: guid(workspace.id, foundryAccount::project.id, roleId)
    scope: workspace
    properties: {
      principalId: foundryAccount::project.identity.principalId
      principalType: 'ServicePrincipal'
      roleDefinitionId: roleId
    }
  }
]

output AZURE_AI_ACCOUNT_NAME string = foundryAccount.name
output AZURE_AI_PROJECT_ID string = foundryAccount::project.id
output AZURE_AI_PROJECT_NAME string = foundryAccount::project.name
output AZURE_AI_MODEL_DEPLOYMENT_NAME string = foundryAccount::modelDeployment.name
output FOUNDRY_PROJECT_ENDPOINT string = 'https://${foundryAccount.name}.services.ai.azure.com/api/projects/${foundryAccount::project.name}'
output APPLICATIONINSIGHTS_RESOURCE_ID string = applicationInsights.id
output LOG_ANALYTICS_WORKSPACE_ID string = workspace.id
