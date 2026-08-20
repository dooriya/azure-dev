// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"azure.ai.evaluation/internal/version"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSummarizeEvaluationResultsCountsMissingResults(t *testing.T) {
	items := []json.RawMessage{
		json.RawMessage(`{"results":[{"name":"relevance","status":"completed","score":4,"passed":true}]}`),
		json.RawMessage(`{"results":[]}`),
	}
	threshold := 3.0
	minPassRate := 0.8
	maxErrorRate := 0.0

	summary, gate, err := summarizeEvaluationResults(
		items,
		[]evaluationEvaluator{
			{
				Name:      "relevance",
				ID:        "builtin.relevance",
				Threshold: &threshold,
				Direction: "increase",
			},
		},
		evaluationQualityGate{
			Enforce:      true,
			MinPassRate:  &minPassRate,
			MaxErrorRate: &maxErrorRate,
		},
		3,
	)
	require.NoError(t, err)
	assert.Equal(t, 3, summary["relevance"].Total)
	assert.Equal(t, 1, summary["relevance"].Passed)
	assert.Equal(t, 2, summary["relevance"].Errored)
	assert.False(t, gate.Passed)
	assert.True(t, gate.Enforced)
}

func TestBuildEvaluationCriteriaUsesJudge(t *testing.T) {
	criteria, err := buildEvaluationCriteria(
		[]evaluationEvaluator{
			{Name: "relevance", ID: "builtin.relevance"},
			{Name: "f1", ID: "builtin.f1_score"},
		},
		evaluationDatasetFields{Query: "query", GroundTruth: "ground_truth"},
		"judge-model",
	)
	require.NoError(t, err)
	assert.Equal(t, "{{item.query}}", criteria[0]["data_mapping"].(map[string]string)["query"])
	assert.Equal(
		t,
		map[string]string{"model": "judge-model"},
		criteria[0]["initialization_parameters"],
	)
	assert.NotContains(t, criteria[1], "initialization_parameters")
}

func TestBuildEvaluationCriteriaUsesJudgeWithCustomMapping(t *testing.T) {
	criteria, err := buildEvaluationCriteria(
		[]evaluationEvaluator{
			{
				Name: "relevance",
				ID:   "builtin.relevance",
				DataMapping: map[string]string{
					"query":    "{{item.custom_query}}",
					"response": "{{sample.output_text}}",
				},
			},
		},
		evaluationDatasetFields{Query: "query", GroundTruth: "ground_truth"},
		"judge-model",
	)
	require.NoError(t, err)
	assert.Equal(
		t,
		map[string]string{"model": "judge-model"},
		criteria[0]["initialization_parameters"],
	)
}

func TestBuildEvaluationCriteriaRequiresCustomMapping(t *testing.T) {
	_, err := buildEvaluationCriteria(
		[]evaluationEvaluator{{Name: "custom", ID: "custom.evaluator"}},
		evaluationDatasetFields{Query: "query", GroundTruth: "ground_truth"},
		"judge-model",
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dataMapping")
}

func TestBuildTargetInputMessagesWithSystemPrompt(t *testing.T) {
	messages := buildTargetInputMessages(
		"  You are a concise assistant.\nUse one sentence.  ",
		"prompt",
	)
	template, ok := messages["template"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, template, 2)
	assert.Equal(t, "system", template[0]["role"])
	assert.Equal(
		t,
		map[string]string{
			"type": "input_text",
			"text": "You are a concise assistant.\nUse one sentence.",
		},
		template[0]["content"],
	)
	assert.Equal(t, "user", template[1]["role"])
	assert.Equal(
		t,
		map[string]string{"type": "input_text", "text": "{{item.prompt}}"},
		template[1]["content"],
	)
}

func TestBuildTargetInputMessagesWithoutSystemPrompt(t *testing.T) {
	messages := buildTargetInputMessages(" \n ", "query")
	template, ok := messages["template"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, template, 1)
	assert.Equal(t, "user", template[0]["role"])
}

func TestSafeEvaluationResultID(t *testing.T) {
	assert.Equal(t, "evalrun_123", safeEvaluationResultID("evalrun_123"))
	assert.Equal(t, "secret", safeEvaluationResultID("../../secret"))
	assert.Equal(t, "evaluation", safeEvaluationResultID(".."))
}

func TestWriteManagedEvaluationResultContract(t *testing.T) {
	root := t.TempDir()
	metadataPath := filepath.Join(root, "results", "azd-evaluation-output-evaluation-efe77b201dc2.json")
	dataset := &foundryDataset{
		ID:      "azureai://dataset/1",
		Name:    "starter-dataset",
		Version: "20260818160000000000",
	}
	run := &foundryEvaluationRun{
		ID:        "evalrun_1",
		Status:    "completed",
		ReportURL: "https://ai.azure.com/report",
		Document:  map[string]any{"id": "evalrun_1", "status": "completed"},
	}

	result, err := writeManagedEvaluationResult(
		managedEvaluationOptions{
			Config:         &evaluationConfig{},
			OutputPath:     filepath.Join(root, "results"),
			ResultMetadata: metadataPath,
		},
		"https://account.services.ai.azure.com/api/projects/project",
		"target-model",
		"judge-model",
		dataset,
		"eval_1",
		run,
		nil,
		nil,
		nil,
	)
	require.NoError(t, err)
	assert.Equal(t, evaluationOutputSchemaVersion, result.SchemaVersion)
	assert.Equal(t, version.Version, result.ExtensionVersion)
	assert.Equal(t, "starter-dataset", result.DatasetName)
	assert.Equal(t, "20260818160000000000", result.DatasetVersion)
	assert.Equal(t, "completed", result.Status)

	content, err := os.ReadFile(metadataPath) //nolint:gosec
	require.NoError(t, err)
	var contract map[string]any
	require.NoError(t, json.Unmarshal(content, &contract))
	assert.Equal(t, float64(evaluationOutputSchemaVersion), contract["schemaVersion"])
	assert.Equal(t, version.Version, contract["extensionVersion"])
	assert.Equal(t, "target-model", contract["targetDeployment"])
	assert.Equal(t, "judge-model", contract["judgeDeployment"])
	assert.Equal(t, "eval_1", contract["evaluationId"])
	assert.Equal(t, "evalrun_1", contract["runId"])
	assert.Equal(t, "https://ai.azure.com/report", contract["reportUrl"])
	assert.NotContains(t, contract, "summaryText")
	assert.NotContains(t, contract, "state")
	require.Len(t, contract, 12)
}

func TestWriteManagedEvaluationSubmittedContract(t *testing.T) {
	root := t.TempDir()
	result, err := writeManagedEvaluationResult(
		managedEvaluationOptions{
			Config:     &evaluationConfig{},
			OutputPath: filepath.Join(root, "results"),
			ResultMetadata: filepath.Join(
				root,
				"results",
				"azd-evaluation-output-evaluation-efe77b201dc2.json",
			),
		},
		"https://account.services.ai.azure.com/api/projects/project",
		"target-model",
		"judge-model",
		&foundryDataset{ID: "dataset-id", Name: "dataset", Version: "1"},
		"eval_1",
		&foundryEvaluationRun{
			ID:       "evalrun_1",
			Status:   "queued",
			Document: map[string]any{"id": "evalrun_1", "status": "queued"},
		},
		nil,
		nil,
		nil,
	)
	require.NoError(t, err)
	assert.Equal(t, "queued", result.Status)
	assert.Empty(t, result.ReportURL)
}
