// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestParseEvaluationServiceConfig(t *testing.T) {
	properties, err := structpb.NewStruct(map[string]any{
		"configFile": "custom-evaluation.yaml",
	})
	require.NoError(t, err)

	config, err := parseEvaluationServiceConfig(&azdext.ServiceConfig{
		Name:                 "evaluation",
		AdditionalProperties: properties,
	})
	require.NoError(t, err)
	assert.Equal(t, "custom-evaluation.yaml", config.ConfigFile)
}

func TestParseEvaluationServiceConfigRejectsMissingConfigFile(t *testing.T) {
	properties, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)

	_, err = parseEvaluationServiceConfig(&azdext.ServiceConfig{
		Name:                 "evaluation",
		AdditionalProperties: properties,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "configFile")
}

func TestRunEvaluationUsesManagedRunner(t *testing.T) {
	root := t.TempDir()
	outputDir := filepath.Join(root, "results")
	resultMetadata := filepath.Join(outputDir, "azd-evaluation-output-evaluation-efe77b201dc2.json")
	resultPath := filepath.Join(outputDir, "remote-run.json")
	var received managedEvaluationOptions
	target := &evaluationServiceTarget{
		managedRunner: func(
			_ context.Context,
			options managedEvaluationOptions,
		) (*evaluationRunResult, error) {
			received = options
			return &evaluationRunResult{
				SchemaVersion:    evaluationOutputSchemaVersion,
				ExtensionVersion: "dev",
				ProjectEndpoint:  "https://example.services.ai.azure.com/api/projects/sample",
				TargetDeployment: "target",
				JudgeDeployment:  "judge",
				DatasetName:      "dataset",
				DatasetVersion:   "1",
				EvaluationID:     "eval_1",
				RunID:            "run_1",
				Status:           "completed",
				ReportURL:        "https://ai.azure.com/report",
				ResultPath:       resultPath,
				SummaryText:      "Evaluation summary:\n  relevance: 1/1 passed",
			}, nil
		},
	}
	files := &resolvedServiceFiles{
		configFile:     filepath.Join(root, "evaluation.yaml"),
		evaluation:     &evaluationConfig{},
		dataset:        filepath.Join(root, "data", "evaluation.jsonl"),
		output:         outputDir,
		resultMetadata: resultMetadata,
	}

	result, err := target.runEvaluation(t.Context(), &azdext.ServiceConfig{
		Environment: map[string]string{
			"FOUNDRY_PROJECT_ENDPOINT": "https://example.services.ai.azure.com/api/projects/sample",
			"FOUNDRY_MODEL_NAME":       "model",
		},
	}, files)
	require.NoError(t, err)
	assert.Equal(t, "https://ai.azure.com/report", result.ReportURL)
	assert.Equal(t, resultPath, result.ResultPath)
	assert.Contains(t, evaluationArtifactNote(result), "relevance: 1/1 passed")
	assert.Same(t, files.evaluation, received.Config)
	assert.Equal(t, files.dataset, received.DatasetPath)
	assert.Equal(t, files.output, received.OutputPath)
	assert.Equal(t, files.resultMetadata, received.ResultMetadata)
	assert.Equal(t, "model", environmentValue(received.Environment, "FOUNDRY_MODEL_NAME"))
}

func TestEvaluationDeployArtifactUsesReport(t *testing.T) {
	result := &evaluationRunResult{
		Status:      "completed",
		ReportURL:   "https://ai.azure.com/report",
		ResultPath:  filepath.Join("results", "remote-run.json"),
		SummaryText: "Evaluation summary:\n  relevance: 1/1 passed",
	}

	artifact := evaluationDeployArtifact(result)

	assert.Equal(t, azdext.ArtifactKind_ARTIFACT_KIND_ENDPOINT, artifact.Kind)
	assert.Equal(t, azdext.LocationKind_LOCATION_KIND_REMOTE, artifact.LocationKind)
	assert.Equal(t, result.ReportURL, artifact.Location)
	assert.Equal(t, "Evaluation report", artifact.Metadata["label"])
	assert.Contains(t, artifact.Metadata["note"], "Local results:")
}

func TestEvaluationDeployArtifactLabelsNoWaitSubmission(t *testing.T) {
	result := &evaluationRunResult{
		Status:     "queued",
		ResultPath: filepath.Join("results", "remote-run.json"),
	}

	artifact := evaluationDeployArtifact(result)

	assert.Equal(t, azdext.ArtifactKind_ARTIFACT_KIND_ENDPOINT, artifact.Kind)
	assert.Equal(t, azdext.LocationKind_LOCATION_KIND_LOCAL, artifact.LocationKind)
	assert.Equal(t, result.ResultPath, artifact.Location)
	assert.Equal(t, "Evaluation submission", artifact.Metadata["label"])
	assert.Contains(t, artifact.Metadata["note"], "without waiting")
}

func TestMergeEnvironmentSkipsEmptyOverrides(t *testing.T) {
	environment := mergeEnvironment(
		[]string{"FOUNDRY_MODEL_NAME=existing", "OTHER=value"},
		map[string]string{"FOUNDRY_MODEL_NAME": "", "NEW": "new-value"},
	)
	assert.Equal(t, "existing", environmentValue(environment, "FOUNDRY_MODEL_NAME"))
	assert.Equal(t, "new-value", environmentValue(environment, "NEW"))
}

func TestSafeResultMetadataName(t *testing.T) {
	assert.Equal(t, "evaluation-service", safeResultMetadataName("evaluation/service"))
	assert.Equal(t, "evaluation", safeResultMetadataName(""))
}

func TestEvaluationOutputFileName(t *testing.T) {
	assert.Equal(
		t,
		"azd-evaluation-output-evaluation-efe77b201dc2.json",
		evaluationOutputFileName("evaluation"),
	)
	assert.Equal(
		t,
		"azd-evaluation-output-evaluation-service-dc04936c5207.json",
		evaluationOutputFileName("evaluation/service"),
	)
}

func TestValidateEvaluationRunResultRejectsUnsupportedSchema(t *testing.T) {
	result := validEvaluationRunResultForTest()
	result.SchemaVersion = 2

	err := validateEvaluationRunResult(result)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "schemaVersion")
}

func TestValidateEvaluationRunResultRejectsMissingRequiredField(t *testing.T) {
	result := validEvaluationRunResultForTest()
	result.RunID = ""

	err := validateEvaluationRunResult(result)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "runId")
}

func validEvaluationRunResultForTest() *evaluationRunResult {
	return &evaluationRunResult{
		SchemaVersion:    evaluationOutputSchemaVersion,
		ExtensionVersion: "0.1.0-preview",
		ProjectEndpoint:  "https://account.services.ai.azure.com/api/projects/project",
		TargetDeployment: "target",
		JudgeDeployment:  "judge",
		DatasetName:      "dataset",
		DatasetVersion:   "1",
		EvaluationID:     "eval_1",
		RunID:            "run_1",
		Status:           "completed",
		ReportURL:        "https://ai.azure.com/report",
		ResultPath:       filepath.Join("results", "remote-run.json"),
	}
}
