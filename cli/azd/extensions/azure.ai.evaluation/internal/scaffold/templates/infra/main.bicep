targetScope = 'subscription'

@minLength(1)
@maxLength(64)
@description('Name of the azd environment.')
param environmentName string

@description('Primary Azure location for all resources.')
param location string

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

var tags = {
  'azd-env-name': environmentName
}

resource resourceGroup 'Microsoft.Resources/resourceGroups@2024-03-01' = {
  name: 'rg-${environmentName}'
  location: location
  tags: tags
}

module resources 'resources.bicep' = {
  name: 'evaluation-resources'
  scope: resourceGroup
  params: {
    environmentName: environmentName
    location: location
    principalId: principalId
    modelName: modelName
    modelVersion: modelVersion
    modelDeploymentName: modelDeploymentName
    modelSku: modelSku
    modelCapacity: modelCapacity
    tags: tags
  }
}

output AZURE_RESOURCE_GROUP string = resourceGroup.name
output AZURE_AI_ACCOUNT_NAME string = resources.outputs.AZURE_AI_ACCOUNT_NAME
output AZURE_AI_PROJECT_ID string = resources.outputs.AZURE_AI_PROJECT_ID
output AZURE_AI_PROJECT_NAME string = resources.outputs.AZURE_AI_PROJECT_NAME
output AZURE_AI_MODEL_DEPLOYMENT_NAME string = resources.outputs.AZURE_AI_MODEL_DEPLOYMENT_NAME
output FOUNDRY_MODEL_NAME string = resources.outputs.AZURE_AI_MODEL_DEPLOYMENT_NAME
output FOUNDRY_JUDGE_MODEL_NAME string = resources.outputs.AZURE_AI_MODEL_DEPLOYMENT_NAME
output FOUNDRY_PROJECT_ENDPOINT string = resources.outputs.FOUNDRY_PROJECT_ENDPOINT
output APPLICATIONINSIGHTS_RESOURCE_ID string = resources.outputs.APPLICATIONINSIGHTS_RESOURCE_ID
output LOG_ANALYTICS_WORKSPACE_ID string = resources.outputs.LOG_ANALYTICS_WORKSPACE_ID
