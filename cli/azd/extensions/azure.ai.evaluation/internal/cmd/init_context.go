// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/cognitiveservices/armcognitiveservices/v2"
	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
)

const (
	useExistingProjectChoice = "Use an existing Foundry project"
	createNewProjectChoice   = "Create a new Foundry project"
)

var invalidDeploymentNameCharacters = regexp.MustCompile(`[^a-z0-9-]+`)
var validEnvironmentName = regexp.MustCompile(`^[a-zA-Z0-9()_.-]{1,64}$`)

type evaluationInitTarget struct {
	ProjectID       string
	ProjectEndpoint string
	ModelDeployment string
	JudgeDeployment string
	SubscriptionID  string
	TenantID        string
	Location        string
	ResourceGroup   string
	AccountName     string
	ProjectName     string
	ModelName       string
	ModelFormat     string
	ModelVersion    string
	ModelSKU        string
	ModelCapacity   int32
}

type evaluationDeployment struct {
	Name      string
	ModelName string
}

func (t *evaluationInitTarget) ExistingProject() bool {
	return t != nil && t.ProjectEndpoint != ""
}

func resolveEvaluationInitTarget(
	ctx context.Context,
	client *azdext.AzdClient,
	flags *initFlags,
	noPrompt bool,
) (*evaluationInitTarget, error) {
	if flags.newProject && flags.projectID != "" {
		return nil, evaluationInitValidationError(
			"--new-project cannot be combined with --project-id",
			"Remove one of the conflicting options and retry.",
		)
	}
	if noPrompt && !flags.newProject && flags.projectID == "" {
		return nil, evaluationInitValidationError(
			"--no-prompt requires --project-id or --new-project",
			"Pass --project-id with --model-deployment to reuse a project, or explicitly pass --new-project.",
		)
	}

	useExisting := flags.projectID != ""
	if !flags.newProject && flags.projectID == "" && !noPrompt {
		response, err := client.Prompt().Select(ctx, &azdext.SelectRequest{
			Options: &azdext.SelectOptions{
				Message: "How do you want to configure Microsoft Foundry?",
				Choices: []*azdext.SelectChoice{
					{Value: "existing", Label: useExistingProjectChoice},
					{Value: "new", Label: createNewProjectChoice},
				},
				SelectedIndex: new(int32(0)),
			},
		})
		if err != nil {
			return nil, fmt.Errorf("selecting Foundry project setup: %w", err)
		}
		useExisting = response.Value != nil && response.GetValue() == 0
	}

	if useExisting {
		return resolveExistingEvaluationTarget(ctx, client, flags, noPrompt)
	}
	return resolveNewEvaluationTarget(ctx, client, flags, noPrompt)
}

func resolveExistingEvaluationTarget(
	ctx context.Context,
	client *azdext.AzdClient,
	flags *initFlags,
	noPrompt bool,
) (*evaluationInitTarget, error) {
	var (
		subscription *azdext.Subscription
		projectID    = strings.TrimSpace(flags.projectID)
	)

	if projectID != "" {
		resourceID, err := parseFoundryProjectID(projectID)
		if err != nil {
			return nil, evaluationInitValidationError(err.Error(), "Provide a valid Foundry project ARM resource ID.")
		}
		if flags.subscriptionID != "" &&
			!strings.EqualFold(flags.subscriptionID, resourceID.SubscriptionID) {
			return nil, evaluationInitValidationError(
				"--subscription does not match --project-id",
				"Use the subscription that owns the Foundry project.",
			)
		}
		tenant, err := client.Account().LookupTenant(ctx, &azdext.LookupTenantRequest{
			SubscriptionId: resourceID.SubscriptionID,
		})
		if err != nil {
			return nil, fmt.Errorf("looking up project subscription tenant: %w", err)
		}
		subscription = &azdext.Subscription{
			Id:           resourceID.SubscriptionID,
			UserTenantId: tenant.GetTenantId(),
		}
	} else {
		if noPrompt {
			return nil, evaluationInitValidationError(
				"existing-project initialization requires --project-id under --no-prompt",
				"Pass --project-id and --model-deployment, or use --new-project.",
			)
		}
		selected, err := selectSubscription(ctx, client, flags.subscriptionID)
		if err != nil {
			return nil, err
		}
		subscription = selected

		projectResponse, err := client.Prompt().PromptSubscriptionResource(
			ctx,
			&azdext.PromptSubscriptionResourceRequest{
				AzureContext: &azdext.AzureContext{Scope: &azdext.AzureScope{
					SubscriptionId: subscription.GetId(),
					TenantId:       subscription.GetUserTenantId(),
				}},
				Options: &azdext.PromptResourceOptions{
					ResourceType:            "Microsoft.CognitiveServices/accounts/projects",
					ResourceTypeDisplayName: "Microsoft Foundry project",
					SelectOptions: &azdext.PromptResourceSelectOptions{
						AllowNewResource: new(false),
						Message:          "Select a Foundry project",
						LoadingMessage:   "Loading Foundry projects...",
					},
				},
			},
		)
		if err != nil {
			return nil, fmt.Errorf("selecting Foundry project: %w", err)
		}
		if projectResponse.GetResource() == nil || projectResponse.GetResource().GetId() == "" {
			return nil, evaluationInitValidationError(
				"Foundry project selection returned no resource",
				"Select an existing Foundry project and retry.",
			)
		}
		projectID = projectResponse.GetResource().GetId()
	}

	credential, err := newEvaluationCredential(subscription.GetUserTenantId())
	if err != nil {
		return nil, err
	}
	project, err := getEvaluationFoundryProject(ctx, credential, projectID)
	if err != nil {
		return nil, err
	}
	project.TenantID = subscription.GetUserTenantId()
	if flags.modelDeployment != "" && (flags.judgeDeployment != "" || noPrompt) {
		project.ModelDeployment = strings.TrimSpace(flags.modelDeployment)
		project.JudgeDeployment = strings.TrimSpace(flags.judgeDeployment)
		if project.JudgeDeployment == "" {
			project.JudgeDeployment = project.ModelDeployment
		}
		return project, nil
	}
	deployments, err := listEvaluationDeployments(ctx, credential, project)
	if err != nil {
		return nil, err
	}
	deployment, err := selectEvaluationDeployment(
		ctx,
		client,
		deployments,
		flags.modelDeployment,
		noPrompt,
		targetDeploymentRole,
		"",
	)
	if err != nil {
		return nil, err
	}
	judgeDeployment := deployment
	if flags.judgeDeployment != "" || !noPrompt {
		judgeDeployment, err = selectEvaluationDeployment(
			ctx,
			client,
			deployments,
			flags.judgeDeployment,
			noPrompt,
			judgeDeploymentRole,
			deployment,
		)
		if err != nil {
			return nil, err
		}
	}

	project.ModelDeployment = deployment
	project.JudgeDeployment = judgeDeployment
	return project, nil
}

func resolveNewEvaluationTarget(
	ctx context.Context,
	client *azdext.AzdClient,
	flags *initFlags,
	noPrompt bool,
) (*evaluationInitTarget, error) {
	if flags.judgeDeployment != "" {
		return nil, evaluationInitValidationError(
			"--judge-model-deployment is not supported with --new-project",
			"New-project mode provisions one deployment; omit the judge option or "+
				"reuse an existing project with a separate judge.",
		)
	}
	target := &evaluationInitTarget{
		SubscriptionID:  strings.TrimSpace(flags.subscriptionID),
		Location:        strings.TrimSpace(flags.location),
		ModelDeployment: strings.TrimSpace(flags.modelDeployment),
	}
	if noPrompt {
		if target.SubscriptionID != "" {
			tenant, err := client.Account().LookupTenant(ctx, &azdext.LookupTenantRequest{
				SubscriptionId: target.SubscriptionID,
			})
			if err != nil {
				return nil, fmt.Errorf("looking up subscription tenant: %w", err)
			}
			target.TenantID = tenant.GetTenantId()
		}
		return target, nil
	}

	subscription, err := selectSubscription(ctx, client, target.SubscriptionID)
	if err != nil {
		return nil, err
	}
	target.SubscriptionID = subscription.GetId()
	target.TenantID = subscription.GetUserTenantId()

	azureContext := &azdext.AzureContext{Scope: &azdext.AzureScope{
		SubscriptionId: target.SubscriptionID,
		TenantId:       target.TenantID,
		Location:       target.Location,
	}}
	if target.Location == "" {
		location, err := client.Prompt().PromptLocation(ctx, &azdext.PromptLocationRequest{
			AzureContext: azureContext,
		})
		if err != nil {
			return nil, fmt.Errorf("selecting Azure location: %w", err)
		}
		target.Location = location.GetLocation().GetName()
		azureContext.Scope.Location = target.Location
	}

	model, err := client.Prompt().PromptAiModel(ctx, &azdext.PromptAiModelRequest{
		AzureContext: azureContext,
		Filter: &azdext.AiModelFilterOptions{
			Locations: []string{target.Location},
			Formats:   []string{"OpenAI"},
		},
		Quota:        &azdext.QuotaCheckOptions{MinRemainingCapacity: 1},
		DefaultValue: "gpt-4.1-mini",
		SelectOptions: &azdext.SelectOptions{
			Message: "Select a model for generation and evaluation",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("selecting evaluation model: %w", err)
	}
	if model.GetModel() == nil {
		return nil, evaluationInitValidationError(
			"model selection returned no model",
			"Select an available chat model and retry.",
		)
	}

	defaultCapacity := int32(10)
	deployment, err := client.Prompt().PromptAiDeployment(ctx, &azdext.PromptAiDeploymentRequest{
		AzureContext: azureContext,
		ModelName:    model.GetModel().GetName(),
		Options: &azdext.AiModelDeploymentOptions{
			Locations: []string{target.Location},
			Capacity:  &defaultCapacity,
		},
		Quota: &azdext.QuotaCheckOptions{MinRemainingCapacity: 1},
	})
	if err != nil {
		return nil, fmt.Errorf("selecting model deployment configuration: %w", err)
	}
	if deployment.GetDeployment() == nil || deployment.GetDeployment().GetSku() == nil {
		return nil, evaluationInitValidationError(
			"model deployment selection returned incomplete configuration",
			"Select a model version, SKU, and capacity and retry.",
		)
	}

	selected := deployment.GetDeployment()
	target.ModelName = selected.GetModelName()
	target.ModelFormat = selected.GetFormat()
	target.ModelVersion = selected.GetVersion()
	target.ModelSKU = selected.GetSku().GetName()
	target.ModelCapacity = selected.GetCapacity()
	if target.ModelDeployment == "" {
		target.ModelDeployment = defaultEvaluationDeploymentName(target.ModelName)
	}
	target.JudgeDeployment = target.ModelDeployment
	return target, nil
}

func selectSubscription(
	ctx context.Context,
	client *azdext.AzdClient,
	subscriptionID string,
) (*azdext.Subscription, error) {
	if subscriptionID != "" {
		tenant, err := client.Account().LookupTenant(ctx, &azdext.LookupTenantRequest{
			SubscriptionId: subscriptionID,
		})
		if err != nil {
			return nil, fmt.Errorf("looking up subscription tenant: %w", err)
		}
		return &azdext.Subscription{
			Id:           subscriptionID,
			UserTenantId: tenant.GetTenantId(),
		}, nil
	}

	response, err := client.Prompt().PromptSubscription(ctx, &azdext.PromptSubscriptionRequest{})
	if err != nil {
		return nil, fmt.Errorf("selecting Azure subscription: %w", err)
	}
	if response.GetSubscription() == nil {
		return nil, evaluationInitValidationError(
			"subscription selection returned no subscription",
			"Select an Azure subscription and retry.",
		)
	}
	return response.GetSubscription(), nil
}

func ensureEvaluationEnvironment(
	ctx context.Context,
	client *azdext.AzdClient,
	preferredName string,
) (*azdext.Environment, error) {
	if preferredName != "" {
		response, err := client.Environment().Get(ctx, &azdext.GetEnvironmentRequest{Name: preferredName})
		if err == nil && response.GetEnvironment() != nil {
			if _, err := client.Environment().Select(ctx, &azdext.SelectEnvironmentRequest{
				Name: preferredName,
			}); err != nil {
				return nil, fmt.Errorf("selecting azd environment %q: %w", preferredName, err)
			}
			return response.GetEnvironment(), nil
		}
	} else {
		response, err := client.Environment().GetCurrent(ctx, &azdext.EmptyRequest{})
		if err == nil && response.GetEnvironment() != nil {
			return response.GetEnvironment(), nil
		}
	}

	if preferredName == "" {
		preferredName = "evaluation"
	}
	_, err := client.Workflow().Run(ctx, &azdext.RunWorkflowRequest{Workflow: &azdext.Workflow{
		Name: "env new",
		Steps: []*azdext.WorkflowStep{
			{Command: &azdext.WorkflowCommand{
				Args: []string{"env", "new", preferredName, "--no-prompt"},
			}},
		},
	}})
	if err != nil {
		return nil, &azdext.LocalError{
			Message:    fmt.Sprintf("Failed to create azd environment %q: %s", preferredName, err),
			Code:       "evaluation_environment_creation_failed",
			Category:   azdext.LocalErrorCategoryDependency,
			Suggestion: "Run 'azd env new' and retry evaluation init.",
		}
	}
	response, err := client.Environment().Get(ctx, &azdext.GetEnvironmentRequest{Name: preferredName})
	if err != nil || response.GetEnvironment() == nil {
		return nil, fmt.Errorf("loading azd environment %q after creation: %w", preferredName, err)
	}
	return response.GetEnvironment(), nil
}

func persistEvaluationInitTarget(
	ctx context.Context,
	client *azdext.AzdClient,
	envName string,
	target *evaluationInitTarget,
) error {
	values := target.environmentValues()
	for _, key := range slices.Sorted(maps.Keys(values)) {
		if values[key] == "" {
			continue
		}
		if _, err := client.Environment().SetValue(ctx, &azdext.SetEnvRequest{
			EnvName: envName,
			Key:     key,
			Value:   values[key],
		}); err != nil {
			return fmt.Errorf("setting %s in azd environment %q: %w", key, envName, err)
		}
	}
	return nil
}

func (t *evaluationInitTarget) environmentValues() map[string]string {
	values := map[string]string{
		"AZURE_TENANT_ID":                t.TenantID,
		"AZURE_SUBSCRIPTION_ID":          t.SubscriptionID,
		"AZURE_LOCATION":                 t.Location,
		"AZURE_AI_DEPLOYMENTS_LOCATION":  t.Location,
		"AZURE_RESOURCE_GROUP":           t.ResourceGroup,
		"AZURE_FOUNDRY_RESOURCE_GROUP":   t.ResourceGroup,
		"AZURE_AI_ACCOUNT_NAME":          t.AccountName,
		"AZURE_AI_PROJECT_NAME":          t.ProjectName,
		"AZURE_AI_PROJECT_ID":            t.ProjectID,
		"FOUNDRY_PROJECT_ENDPOINT":       t.ProjectEndpoint,
		"AZURE_AI_PROJECT_ENDPOINT":      t.ProjectEndpoint,
		"AZURE_AI_MODEL_NAME":            t.ModelName,
		"AZURE_AI_MODEL_VERSION":         t.ModelVersion,
		"AZURE_AI_MODEL_DEPLOYMENT_NAME": t.ModelDeployment,
		"FOUNDRY_MODEL_NAME":             t.ModelDeployment,
		"FOUNDRY_JUDGE_MODEL_NAME":       t.JudgeDeployment,
		"AZURE_AI_MODEL_SKU":             t.ModelSKU,
	}
	if t.ModelCapacity > 0 {
		values["AZURE_AI_MODEL_CAPACITY"] = strconv.Itoa(int(t.ModelCapacity))
	}
	if t.AccountName != "" {
		values["AZURE_OPENAI_ENDPOINT"] = fmt.Sprintf(
			"https://%s.openai.azure.com/",
			t.AccountName,
		)
	}
	return values
}

func evaluationInitValidationError(message, suggestion string) error {
	return &azdext.LocalError{
		Message:    message,
		Code:       "evaluation_invalid_init_options",
		Category:   azdext.LocalErrorCategoryValidation,
		Suggestion: suggestion,
	}
}

func validateEvaluationEnvironmentName(name string) error {
	if name == "." ||
		name == ".." ||
		filepath.IsAbs(name) ||
		filepath.VolumeName(name) != "" ||
		filepath.Base(name) != name ||
		!validEnvironmentName.MatchString(name) {
		return evaluationInitValidationError(
			fmt.Sprintf("invalid azd environment name %q", name),
			"Use a single environment name containing only letters, numbers, hyphens, underscores, dots, or parentheses.",
		)
	}
	return nil
}

func rejectNestedAzdProject(target string) error {
	current := filepath.Dir(filepath.Clean(target))
	for {
		for _, manifest := range []string{"azure.yaml", "azure.yml"} {
			if _, err := os.Stat(filepath.Join(current, manifest)); err == nil {
				return evaluationInitValidationError(
					fmt.Sprintf("target directory is nested under the azd project at %q", current),
					"Run evaluation init at that project root or choose a directory outside the existing azd project.",
				)
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("inspecting parent azd project: %w", err)
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

func newEvaluationCredential(tenantID string) (azcore.TokenCredential, error) {
	return newAzdTokenCredential(tenantID), nil
}

func parseFoundryProjectID(projectID string) (*arm.ResourceID, error) {
	resourceID, err := arm.ParseResourceID(projectID)
	if err != nil {
		return nil, fmt.Errorf("invalid Foundry project resource ID: %w", err)
	}
	resourceType := resourceID.ResourceType
	if !strings.EqualFold(resourceType.Namespace, "Microsoft.CognitiveServices") ||
		len(resourceType.Types) != 2 ||
		!strings.EqualFold(resourceType.Types[0], "accounts") ||
		!strings.EqualFold(resourceType.Types[1], "projects") {
		return nil, fmt.Errorf(
			"resource ID must identify Microsoft.CognitiveServices/accounts/projects",
		)
	}
	return resourceID, nil
}

func getEvaluationFoundryProject(
	ctx context.Context,
	credential azcore.TokenCredential,
	projectID string,
) (*evaluationInitTarget, error) {
	resourceID, err := parseFoundryProjectID(projectID)
	if err != nil {
		return nil, err
	}
	if resourceID.Parent == nil {
		return nil, fmt.Errorf("Foundry project resource ID is missing its account parent")
	}
	accountName := resourceID.Parent.Name
	projectName := resourceID.Name
	projects, err := armcognitiveservices.NewProjectsClient(
		resourceID.SubscriptionID,
		credential,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("creating Foundry projects client: %w", err)
	}
	response, err := projects.Get(
		ctx,
		resourceID.ResourceGroupName,
		accountName,
		projectName,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("getting Foundry project %q: %w", projectName, err)
	}
	location := ""
	if response.Location != nil {
		location = *response.Location
	}
	return &evaluationInitTarget{
		ProjectID: projectID,
		ProjectEndpoint: fmt.Sprintf(
			"https://%s.services.ai.azure.com/api/projects/%s",
			accountName,
			projectName,
		),
		SubscriptionID: resourceID.SubscriptionID,
		Location:       location,
		ResourceGroup:  resourceID.ResourceGroupName,
		AccountName:    accountName,
		ProjectName:    projectName,
	}, nil
}

func listEvaluationDeployments(
	ctx context.Context,
	credential azcore.TokenCredential,
	project *evaluationInitTarget,
) ([]evaluationDeployment, error) {
	client, err := newFoundryEvaluationClient(project.ProjectEndpoint, credential)
	if err != nil {
		return nil, fmt.Errorf("creating Foundry deployments client: %w", err)
	}
	deployments, err := client.listDeployments(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing project model deployments: %w", err)
	}
	var results []evaluationDeployment
	for _, deployment := range deployments {
		if deployment.Name != "" &&
			strings.EqualFold(deployment.Type, "ModelDeployment") &&
			supportsGenerativeEvaluation(deployment.Capabilities) {
			results = append(results, evaluationDeployment{
				Name:      deployment.Name,
				ModelName: deployment.ModelName,
			})
		}
	}
	slices.SortFunc(results, func(a, b evaluationDeployment) int {
		return cmp.Compare(a.Name, b.Name)
	})
	return results, nil
}

func supportsGenerativeEvaluation(capabilities map[string]string) bool {
	for capability, value := range capabilities {
		normalized := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(capability))
		if strings.EqualFold(value, "true") {
			switch normalized {
			case "agentsv2", "chat", "chatcompletion", "responses":
				return true
			}
		}
	}
	return false
}

type evaluationDeploymentRole struct {
	name          string
	promptMessage string
	requiredFlag  string
}

var (
	targetDeploymentRole = evaluationDeploymentRole{
		name:          "target model deployment",
		promptMessage: "Select the target model deployment (generates responses to evaluate)",
		requiredFlag:  "--model-deployment",
	}
	judgeDeploymentRole = evaluationDeploymentRole{
		name:          "judge model deployment",
		promptMessage: "Select the judge model deployment (scores responses with AI-assisted evaluators)",
		requiredFlag:  "--judge-model-deployment",
	}
)

func selectEvaluationDeployment(
	ctx context.Context,
	client *azdext.AzdClient,
	deployments []evaluationDeployment,
	requested string,
	noPrompt bool,
	role evaluationDeploymentRole,
	defaultName string,
) (string, error) {
	if requested != "" {
		for _, deployment := range deployments {
			if strings.EqualFold(requested, deployment.Name) {
				return deployment.Name, nil
			}
		}
		return "", evaluationInitValidationError(
			fmt.Sprintf("%s %q was not found in the selected Foundry account", role.name, requested),
			fmt.Sprintf("Choose an existing %s and retry.", role.name),
		)
	}
	if len(deployments) == 0 {
		return "", evaluationInitValidationError(
			"the selected Foundry account has no model deployments",
			"Deploy a chat model in Foundry, then retry evaluation init.",
		)
	}
	if noPrompt {
		return "", evaluationInitValidationError(
			fmt.Sprintf("%s is required under --no-prompt", role.requiredFlag),
			fmt.Sprintf("Pass the name of an existing %s.", role.name),
		)
	}
	response, err := client.Prompt().Select(ctx, &azdext.SelectRequest{
		Options: &azdext.SelectOptions{
			Message:       role.promptMessage,
			Choices:       deploymentChoices(deployments),
			SelectedIndex: new(deploymentIndex(deployments, defaultName)),
		},
	})
	if err != nil {
		return "", fmt.Errorf("selecting %s: %w", role.name, err)
	}

	if response.Value == nil ||
		int(response.GetValue()) < 0 ||
		int(response.GetValue()) >= len(deployments) {
		return "", evaluationInitValidationError(
			fmt.Sprintf("%s selection returned an invalid result", role.name),
			fmt.Sprintf("Select a %s and retry.", role.name),
		)
	}
	return deployments[int(response.GetValue())].Name, nil
}

func deploymentIndex(deployments []evaluationDeployment, name string) int32 {
	for index, deployment := range deployments {
		if strings.EqualFold(deployment.Name, name) {
			return int32(index) //nolint:gosec // Deployment lists are bounded by the service response.
		}
	}
	return 0
}

func deploymentChoices(deployments []evaluationDeployment) []*azdext.SelectChoice {
	choices := make([]*azdext.SelectChoice, 0, len(deployments))
	for _, deployment := range deployments {
		label := deployment.Name
		if deployment.ModelName != "" && !strings.EqualFold(deployment.Name, deployment.ModelName) {
			label = fmt.Sprintf("%s (%s)", deployment.Name, deployment.ModelName)
		}
		choices = append(choices, &azdext.SelectChoice{
			Value: deployment.Name,
			Label: label,
		})
	}
	return choices
}

func defaultEvaluationDeploymentName(modelName string) string {
	name := strings.ToLower(strings.TrimSpace(modelName))
	name = strings.ReplaceAll(name, ".", "-")
	name = invalidDeploymentNameCharacters.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if name == "" {
		return "evaluation-model"
	}
	return name
}
