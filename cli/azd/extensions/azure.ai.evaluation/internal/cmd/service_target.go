// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
)

var _ azdext.ServiceTargetProvider = (*evaluationServiceTarget)(nil)

type evaluationServiceConfig struct {
	Script         string `json:"script"`
	Dataset        string `json:"dataset"`
	Results        string `json:"results"`
	DatasetName    string `json:"datasetName"`
	DatasetVersion string `json:"datasetVersion"`
	Python         string `json:"python"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
	NoWait         bool   `json:"noWait"`
	Tracing        *bool  `json:"tracing"`
	CaptureContent bool   `json:"captureContent"`
}

func defaultEvaluationServiceConfig() evaluationServiceConfig {
	return evaluationServiceConfig{
		Script:         filepath.Join("src", "evaluate.py"),
		Dataset:        filepath.Join("data", "evaluation.jsonl"),
		Results:        "results",
		DatasetName:    "model-evaluation",
		DatasetVersion: "auto",
		TimeoutSeconds: 3600,
		Tracing:        new(true),
	}
}

type evaluationServiceTarget struct {
	azdClient *azdext.AzdClient
	runner    processRunner
}

func newEvaluationServiceTarget(client *azdext.AzdClient) azdext.ServiceTargetProvider {
	return &evaluationServiceTarget{
		azdClient: client,
		runner: &execProcessRunner{
			stdout: os.Stdout,
			stderr: os.Stderr,
		},
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
	return nil, nil
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
	files, err := p.resolveServiceFiles(ctx, serviceConfig)
	if err != nil {
		return nil, err
	}
	if progress != nil {
		progress("Validated evaluation project")
	}
	return &azdext.ServicePackageResult{
		Artifacts: []*azdext.Artifact{
			{
				Kind:         azdext.ArtifactKind_ARTIFACT_KIND_DIRECTORY,
				Location:     files.serviceRoot,
				LocationKind: azdext.LocationKind_LOCATION_KIND_LOCAL,
			},
		},
	}, nil
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
	if err := p.runEvaluation(ctx, serviceConfig, files); err != nil {
		return nil, err
	}
	if progress != nil {
		progress("Managed Foundry evaluation completed")
	}
	return &azdext.ServiceDeployResult{
		Artifacts: []*azdext.Artifact{
			{
				Kind:         azdext.ArtifactKind_ARTIFACT_KIND_DIRECTORY,
				Location:     files.results,
				LocationKind: azdext.LocationKind_LOCATION_KIND_LOCAL,
			},
		},
	}, nil
}

type resolvedServiceFiles struct {
	config      evaluationServiceConfig
	serviceRoot string
	script      string
	dataset     string
	results     string
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
	script, err := joinProjectPath(serviceRoot, config.Script)
	if err != nil {
		return nil, evaluationConfigError(fmt.Sprintf("evaluation script path is invalid: %s", err))
	}
	dataset, err := joinProjectPath(serviceRoot, config.Dataset)
	if err != nil {
		return nil, evaluationConfigError(fmt.Sprintf("evaluation dataset path is invalid: %s", err))
	}
	results, err := joinProjectPath(serviceRoot, config.Results)
	if err != nil {
		return nil, evaluationConfigError(fmt.Sprintf("evaluation results path is invalid: %s", err))
	}

	for label, path := range map[string]string{"script": script, "dataset": dataset} {
		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil, evaluationConfigError(fmt.Sprintf("evaluation %s %q is unavailable: %s", label, path, statErr))
		}
		if info.IsDir() {
			return nil, evaluationConfigError(fmt.Sprintf("evaluation %s %q must be a file", label, path))
		}
	}

	return &resolvedServiceFiles{
		config:      config,
		serviceRoot: serviceRoot,
		script:      script,
		dataset:     dataset,
		results:     results,
	}, nil
}

func parseEvaluationServiceConfig(serviceConfig *azdext.ServiceConfig) (evaluationServiceConfig, error) {
	config := defaultEvaluationServiceConfig()
	properties := serviceConfig.GetAdditionalProperties()
	if properties == nil {
		return config, nil
	}

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

	switch {
	case strings.TrimSpace(config.Script) == "":
		return config, evaluationConfigError("evaluation service 'script' cannot be empty")
	case strings.TrimSpace(config.Dataset) == "":
		return config, evaluationConfigError("evaluation service 'dataset' cannot be empty")
	case strings.TrimSpace(config.Results) == "":
		return config, evaluationConfigError("evaluation service 'results' cannot be empty")
	case strings.TrimSpace(config.DatasetName) == "":
		return config, evaluationConfigError("evaluation service 'datasetName' cannot be empty")
	case strings.TrimSpace(config.DatasetVersion) == "":
		return config, evaluationConfigError("evaluation service 'datasetVersion' cannot be empty")
	case config.TimeoutSeconds <= 0:
		return config, evaluationConfigError("evaluation service 'timeoutSeconds' must be greater than zero")
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
) error {
	python, err := resolvePython(files.serviceRoot, files.config.Python)
	if err != nil {
		return err
	}

	args := []string{
		files.script,
		"--remote",
		"--dataset", files.dataset,
		"--output-dir", files.results,
		"--dataset-name", files.config.DatasetName,
		"--dataset-version", files.config.DatasetVersion,
		"--timeout-seconds", strconv.Itoa(files.config.TimeoutSeconds),
	}
	if files.config.NoWait {
		args = append(args, "--no-wait")
	}
	if files.config.Tracing != nil && !*files.config.Tracing {
		args = append(args, "--no-tracing")
	}
	if files.config.CaptureContent {
		args = append(args, "--capture-content")
	}

	environment := mergeEnvironment(os.Environ(), serviceConfig.GetEnvironment())
	if environmentValue(environment, "FOUNDRY_PROJECT_ENDPOINT", "AZURE_AI_PROJECT_ENDPOINT") == "" {
		return &azdext.LocalError{
			Message:    "Foundry project endpoint is not configured.",
			Code:       "evaluation_project_endpoint_missing",
			Category:   azdext.LocalErrorCategoryDependency,
			Suggestion: "Run 'azd ai evaluation provision', or set FOUNDRY_PROJECT_ENDPOINT in the active azd environment.",
		}
	}
	if environmentValue(environment, "FOUNDRY_MODEL_NAME", "AZURE_AI_MODEL_DEPLOYMENT_NAME") == "" {
		return &azdext.LocalError{
			Message:    "Foundry model deployment is not configured.",
			Code:       "evaluation_model_deployment_missing",
			Category:   azdext.LocalErrorCategoryDependency,
			Suggestion: "Run 'azd ai evaluation provision', or set FOUNDRY_MODEL_NAME in the active azd environment.",
		}
	}

	err = p.runner.Run(ctx, processSpec{
		executable:  python,
		args:        args,
		directory:   files.serviceRoot,
		environment: environment,
	})
	if err != nil {
		return &azdext.LocalError{
			Message:  fmt.Sprintf("Managed evaluation failed: %s", err),
			Code:     "evaluation_remote_run_failed",
			Category: azdext.LocalErrorCategoryDependency,
			Suggestion: "Review the Python error above, install requirements.txt, and verify your Foundry access. " +
				"Use datasetVersion: auto or choose an unused explicit version.",
		}
	}
	return nil
}

func resolvePython(serviceRoot, configured string) (string, error) {
	if configured != "" {
		if path, err := exec.LookPath(configured); err == nil {
			return path, nil
		}
		return "", pythonNotFoundError(configured)
	}

	venvCandidates := []string{
		filepath.Join(serviceRoot, ".venv", "bin", "python"),
		filepath.Join(serviceRoot, ".venv", "Scripts", "python.exe"),
	}
	for _, candidate := range venvCandidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}

	candidates := []string{"python3", "python"}
	if runtime.GOOS == "windows" {
		candidates = []string{"python", "py"}
	}
	for _, candidate := range candidates {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", pythonNotFoundError(strings.Join(candidates, " or "))
}

func pythonNotFoundError(name string) error {
	return &azdext.LocalError{
		Message:    fmt.Sprintf("Could not find Python (%s).", name),
		Code:       "evaluation_python_not_found",
		Category:   azdext.LocalErrorCategoryDependency,
		Suggestion: "Install Python 3.10 or later, or set services.evaluation.python in azure.yaml.",
	}
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

type processSpec struct {
	executable  string
	args        []string
	directory   string
	environment []string
}

type processRunner interface {
	Run(ctx context.Context, spec processSpec) error
}

type execProcessRunner struct {
	stdout io.Writer
	stderr io.Writer
}

func (r *execProcessRunner) Run(ctx context.Context, spec processSpec) error {
	// The executable is resolved through exec.LookPath or a scaffold-local virtual environment.
	command := exec.CommandContext(ctx, spec.executable, spec.args...) //nolint:gosec
	command.Dir = spec.directory
	command.Env = spec.environment
	command.Stdout = r.stdout
	command.Stderr = r.stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("running %s: %w", filepath.Base(spec.executable), err)
	}
	return nil
}
