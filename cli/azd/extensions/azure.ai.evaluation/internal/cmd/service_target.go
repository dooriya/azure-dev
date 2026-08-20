// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
)

var _ azdext.ServiceTargetProvider = (*evaluationServiceTarget)(nil)

type evaluationServiceConfig struct {
	ConfigFile string `json:"configFile"`
}

type evaluationServiceTarget struct {
	azdClient     *azdext.AzdClient
	managedRunner func(context.Context, managedEvaluationOptions) (*evaluationRunResult, error)
}

func newEvaluationServiceTarget(client *azdext.AzdClient) azdext.ServiceTargetProvider {
	return &evaluationServiceTarget{
		azdClient:     client,
		managedRunner: runManagedEvaluation,
	}
}

func (p *evaluationServiceTarget) Initialize(
	ctx context.Context,
	serviceConfig *azdext.ServiceConfig,
) error {
	return nil
}

func (p *evaluationServiceTarget) Endpoints(
	ctx context.Context,
	serviceConfig *azdext.ServiceConfig,
	targetResource *azdext.TargetResource,
) ([]string, error) {
	files, err := p.resolveServiceFiles(ctx, serviceConfig)
	if err != nil {
		return nil, err
	}
	result, err := readEvaluationRunResult(files.resultMetadata)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if result.ReportURL == "" {
		return nil, nil
	}
	return []string{result.ReportURL}, nil
}

func (p *evaluationServiceTarget) GetTargetResource(
	ctx context.Context,
	subscriptionID string,
	serviceConfig *azdext.ServiceConfig,
	defaultResolver func() (*azdext.TargetResource, error),
) (*azdext.TargetResource, error) {
	if defaultResolver != nil {
		if target, err := defaultResolver(); err == nil && target != nil {
			return target, nil
		}
	}
	return &azdext.TargetResource{SubscriptionId: subscriptionID}, nil
}

func (p *evaluationServiceTarget) Package(
	ctx context.Context,
	serviceConfig *azdext.ServiceConfig,
	serviceContext *azdext.ServiceContext,
	progress azdext.ProgressReporter,
) (*azdext.ServicePackageResult, error) {
	_, err := p.resolveServiceFiles(ctx, serviceConfig)
	if err != nil {
		return nil, err
	}
	if progress != nil {
		progress("Validated evaluation project")
	}
	return &azdext.ServicePackageResult{}, nil
}

func (p *evaluationServiceTarget) Publish(
	ctx context.Context,
	serviceConfig *azdext.ServiceConfig,
	serviceContext *azdext.ServiceContext,
	targetResource *azdext.TargetResource,
	publishOptions *azdext.PublishOptions,
	progress azdext.ProgressReporter,
) (*azdext.ServicePublishResult, error) {
	return &azdext.ServicePublishResult{}, nil
}

func (p *evaluationServiceTarget) Deploy(
	ctx context.Context,
	serviceConfig *azdext.ServiceConfig,
	serviceContext *azdext.ServiceContext,
	targetResource *azdext.TargetResource,
	progress azdext.ProgressReporter,
) (*azdext.ServiceDeployResult, error) {
	files, err := p.resolveServiceFiles(ctx, serviceConfig)
	if err != nil {
		return nil, err
	}
	if progress != nil {
		progress("Starting managed Foundry evaluation")
	}
	result, err := p.runEvaluation(ctx, serviceConfig, files)
	if err != nil {
		return nil, err
	}
	if progress != nil {
		if !isTerminalEvaluationStatus(result.Status) {
			progress("Managed Foundry evaluation submitted")
		} else {
			progress("Managed Foundry evaluation completed")
		}
	}
	return &azdext.ServiceDeployResult{Artifacts: []*azdext.Artifact{
		evaluationDeployArtifact(result),
	}}, nil
}

func evaluationDeployArtifact(result *evaluationRunResult) *azdext.Artifact {
	if !isTerminalEvaluationStatus(result.Status) {
		return &azdext.Artifact{
			Kind:         azdext.ArtifactKind_ARTIFACT_KIND_ENDPOINT,
			Location:     result.ResultPath,
			LocationKind: azdext.LocationKind_LOCATION_KIND_LOCAL,
			Metadata: map[string]string{
				"label": "Evaluation submission",
				"note": "Managed run submitted without waiting. " +
					"The Foundry report URL becomes available after completion.",
			},
		}
	}
	if result.ReportURL == "" {
		return &azdext.Artifact{
			Kind:         azdext.ArtifactKind_ARTIFACT_KIND_ENDPOINT,
			Location:     result.ResultPath,
			LocationKind: azdext.LocationKind_LOCATION_KIND_LOCAL,
			Metadata: map[string]string{
				"label": "Evaluation results",
				"note":  result.SummaryText,
			},
		}
	}
	return &azdext.Artifact{
		Kind:         azdext.ArtifactKind_ARTIFACT_KIND_ENDPOINT,
		Location:     result.ReportURL,
		LocationKind: azdext.LocationKind_LOCATION_KIND_REMOTE,
		Metadata: map[string]string{
			"label": "Evaluation report",
			"note":  evaluationArtifactNote(result),
		},
	}
}

type resolvedServiceFiles struct {
	configFile     string
	evaluation     *evaluationConfig
	dataset        string
	output         string
	resultMetadata string
}

func (p *evaluationServiceTarget) resolveServiceFiles(
	ctx context.Context,
	serviceConfig *azdext.ServiceConfig,
) (*resolvedServiceFiles, error) {
	if serviceConfig == nil {
		return nil, evaluationConfigError("evaluation service configuration is missing")
	}
	config, err := parseEvaluationServiceConfig(serviceConfig)
	if err != nil {
		return nil, err
	}

	project, err := p.azdClient.Project().Get(ctx, &azdext.EmptyRequest{})
	if err != nil {
		return nil, &azdext.LocalError{
			Message:    fmt.Sprintf("Failed to load the azd project: %s", err),
			Code:       "evaluation_project_not_found",
			Category:   azdext.LocalErrorCategoryDependency,
			Suggestion: "Run 'azd ai evaluation init' from an evaluation project directory.",
		}
	}
	if project.GetProject() == nil {
		return nil, evaluationConfigError("azd project configuration is missing")
	}

	servicePath := serviceConfig.GetRelativePath()
	if servicePath == "" {
		servicePath = "."
	}
	serviceRoot, err := joinProjectPath(project.GetProject().GetPath(), servicePath)
	if err != nil {
		return nil, evaluationConfigError(
			fmt.Sprintf("evaluation service path is invalid: %s", err),
		)
	}
	configFile, err := joinProjectPath(serviceRoot, config.ConfigFile)
	if err != nil {
		return nil, evaluationConfigError(fmt.Sprintf("evaluation config path is invalid: %s", err))
	}
	info, err := os.Stat(configFile)
	if err != nil {
		return nil, evaluationConfigError(fmt.Sprintf("evaluation config %q is unavailable: %s", configFile, err))
	}
	if info.IsDir() {
		return nil, evaluationConfigError(fmt.Sprintf("evaluation config %q must be a file", configFile))
	}
	evaluation, err := loadEvaluationConfig(configFile)
	if err != nil {
		return nil, err
	}
	dataset, output, err := evaluation.resolvePaths(configFile)
	if err != nil {
		return nil, err
	}
	datasetInfo, err := os.Stat(dataset)
	if err != nil {
		return nil, evaluationConfigError(fmt.Sprintf("evaluation dataset %q is unavailable: %s", dataset, err))
	}
	if datasetInfo.IsDir() {
		return nil, evaluationConfigError(fmt.Sprintf("evaluation dataset %q must be a file", dataset))
	}

	return &resolvedServiceFiles{
		configFile:     configFile,
		evaluation:     evaluation,
		dataset:        dataset,
		output:         output,
		resultMetadata: filepath.Join(output, evaluationOutputFileName(serviceConfig.GetName())),
	}, nil
}

func parseEvaluationServiceConfig(serviceConfig *azdext.ServiceConfig) (evaluationServiceConfig, error) {
	var config evaluationServiceConfig
	properties := serviceConfig.GetAdditionalProperties()
	if properties != nil {
		payload, err := json.Marshal(properties.AsMap())
		if err != nil {
			return config, evaluationConfigError(
				fmt.Sprintf("failed to encode evaluation service %q: %s", serviceConfig.GetName(), err),
			)
		}
		if err := json.Unmarshal(payload, &config); err != nil {
			return config, evaluationConfigError(
				fmt.Sprintf("failed to parse evaluation service %q: %s", serviceConfig.GetName(), err),
			)
		}
	}

	if strings.TrimSpace(config.ConfigFile) == "" {
		return config, evaluationConfigError("evaluation service 'configFile' cannot be empty")
	}
	return config, nil
}

func evaluationConfigError(message string) error {
	return &azdext.LocalError{
		Message:    message,
		Code:       "evaluation_invalid_service_config",
		Category:   azdext.LocalErrorCategoryValidation,
		Suggestion: "Fix the azure.ai.evaluation service in azure.yaml and retry.",
	}
}

func (p *evaluationServiceTarget) runEvaluation(
	ctx context.Context,
	serviceConfig *azdext.ServiceConfig,
	files *resolvedServiceFiles,
) (*evaluationRunResult, error) {
	environment := mergeEnvironment(os.Environ(), serviceConfig.GetEnvironment())
	if err := os.Remove(files.resultMetadata); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("clearing prior evaluation deployment result: %w", err)
	}
	runner := p.managedRunner
	if runner == nil {
		runner = runManagedEvaluation
	}
	result, err := runner(ctx, managedEvaluationOptions{
		Config:         files.evaluation,
		DatasetPath:    files.dataset,
		OutputPath:     files.output,
		ResultMetadata: files.resultMetadata,
		Environment:    environment,
	})
	if err != nil {
		resultDetails := ""
		if result == nil {
			result, _ = readEvaluationRunResult(files.resultMetadata)
		}
		if result != nil {
			if result.ReportURL != "" {
				resultDetails += fmt.Sprintf(" Foundry report: %s.", result.ReportURL)
			}
			if result.ResultPath != "" {
				resultDetails += fmt.Sprintf(" Local results: %s.", result.ResultPath)
			}
		}
		return nil, &azdext.LocalError{
			Message:  fmt.Sprintf("Managed evaluation failed: %s%s", err, resultDetails),
			Code:     "evaluation_remote_run_failed",
			Category: azdext.LocalErrorCategoryDependency,
			Suggestion: "Review the evaluation error and generated result above. " +
				"Verify evaluation.yaml and Foundry access before retrying.",
		}
	}
	return result, nil
}

func evaluationArtifactNote(result *evaluationRunResult) string {
	localResult := fmt.Sprintf("Local results: %s", result.ResultPath)
	if strings.TrimSpace(result.SummaryText) == "" {
		return localResult
	}
	summary := strings.ReplaceAll(result.SummaryText, "\n", "\n  ")
	return summary + "\n  " + localResult
}

func readEvaluationRunResult(metadataPath string) (*evaluationRunResult, error) {
	content, err := os.ReadFile(metadataPath) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("reading evaluation deployment result: %w", err)
	}
	var result evaluationRunResult
	if err := json.Unmarshal(content, &result); err != nil {
		return nil, fmt.Errorf("parsing evaluation deployment result: %w", err)
	}
	if err := validateEvaluationRunResult(&result); err != nil {
		return nil, fmt.Errorf("validating evaluation deployment result: %w", err)
	}
	return &result, nil
}

func validateEvaluationRunResult(result *evaluationRunResult) error {
	if result.SchemaVersion != evaluationOutputSchemaVersion {
		return fmt.Errorf(
			"unsupported schemaVersion %d; expected %d",
			result.SchemaVersion,
			evaluationOutputSchemaVersion,
		)
	}
	required := map[string]string{
		"extensionVersion": result.ExtensionVersion,
		"projectEndpoint":  result.ProjectEndpoint,
		"targetDeployment": result.TargetDeployment,
		"judgeDeployment":  result.JudgeDeployment,
		"datasetName":      result.DatasetName,
		"datasetVersion":   result.DatasetVersion,
		"evaluationId":     result.EvaluationID,
		"runId":            result.RunID,
		"status":           result.Status,
		"resultPath":       result.ResultPath,
	}
	for field, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s cannot be empty", field)
		}
	}
	projectEndpoint, err := validateFoundryProjectEndpoint(result.ProjectEndpoint)
	if err != nil {
		return fmt.Errorf("projectEndpoint is invalid: %w", err)
	}
	reportURL, err := validateEvaluationReportURL(result.ReportURL)
	if err != nil {
		return err
	}
	result.ProjectEndpoint = projectEndpoint
	result.ReportURL = reportURL
	return nil
}

func validateEvaluationReportURL(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("parsing Foundry report URL: %w", err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		return "", fmt.Errorf("Foundry report URL must use HTTPS")
	}
	return parsed.String(), nil
}

func mergeEnvironment(base []string, overrides map[string]string) []string {
	values := make(map[string]string, len(base)+len(overrides))
	order := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		normalized := environmentKey(key)
		if _, exists := values[normalized]; !exists {
			order = append(order, normalized)
		}
		values[normalized] = key + "=" + value
	}
	for key, value := range overrides {
		if value == "" {
			continue
		}
		normalized := environmentKey(key)
		if _, exists := values[normalized]; !exists {
			order = append(order, normalized)
		}
		values[normalized] = key + "=" + value
	}

	result := make([]string, 0, len(order))
	for _, key := range order {
		result = append(result, values[key])
	}
	return result
}

func environmentKey(key string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(key)
	}
	return key
}

func environmentValue(environment []string, names ...string) string {
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[environmentKey(key)] = value
		}
	}
	for _, name := range names {
		if value := strings.TrimSpace(values[environmentKey(name)]); value != "" {
			return value
		}
	}
	return ""
}

func safeResultMetadataName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "evaluation"
	}
	return strings.Map(func(character rune) rune {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '-',
			character == '_':
			return character
		default:
			return '-'
		}
	}, value)
}

func evaluationOutputFileName(serviceName string) string {
	hash := sha256.Sum256([]byte(serviceName))
	return fmt.Sprintf(
		"azd-evaluation-output-%s-%x.json",
		safeResultMetadataName(serviceName),
		hash[:6],
	)
}
