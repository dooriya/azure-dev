// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"encoding/json"
	"testing"

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

func TestSafeEvaluationResultID(t *testing.T) {
	assert.Equal(t, "evalrun_123", safeEvaluationResultID("evalrun_123"))
	assert.Equal(t, "secret", safeEvaluationResultID("../../secret"))
	assert.Equal(t, "evaluation", safeEvaluationResultID(".."))
}
