// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validEvaluationConfigForTest = `version: 1
name: test
dataset:
  path: data/evaluation.jsonl
  format: jsonl
  name: test-dataset
  version: auto
  fields:
    query: prompt
    groundTruth: expected
target:
  model: ${FOUNDRY_MODEL_NAME}
  systemPrompt: |
    You are a concise technical assistant.
    Use one sentence.
evaluators:
  - name: relevance
    id: builtin.relevance
    threshold: 3
qualityGate:
  enforce: false
  minPassRate: 0.8
  maxErrorRate: 0
remote:
  timeoutSeconds: 30
  pollSeconds: 1
  noWait: false
output:
  path: results
`

func TestLoadEvaluationConfig(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "data"), 0o750))
	configPath := filepath.Join(root, "evaluation.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(validEvaluationConfigForTest), 0o600))

	config, err := loadEvaluationConfig(configPath)
	require.NoError(t, err)
	require.NotNil(t, config.Target.Sampling.TopP)
	assert.Equal(t, 1.0, *config.Target.Sampling.TopP)
	require.NotNil(t, config.Target.Sampling.MaxCompletionTokens)
	assert.Equal(t, 2048, *config.Target.Sampling.MaxCompletionTokens)
	assert.Equal(
		t,
		"You are a concise technical assistant.\nUse one sentence.\n",
		config.Target.SystemPrompt,
	)
	assert.Equal(t, "increase", config.Evaluators[0].Direction)
	datasetPath, outputPath, err := config.resolvePaths(configPath)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "data", "evaluation.jsonl"), datasetPath)
	assert.Equal(t, filepath.Join(root, "results"), outputPath)
}

func TestLoadEvaluationConfigRejectsExplicitInvalidValues(t *testing.T) {
	tests := []struct {
		name         string
		oldValue     string
		invalidValue string
		message      string
	}{
		{
			name:         "zero timeout",
			oldValue:     "timeoutSeconds: 30",
			invalidValue: "timeoutSeconds: 0",
			message:      "timeoutSeconds",
		},
		{
			name:         "empty output path",
			oldValue:     "path: results",
			invalidValue: `path: ""`,
			message:      "output.path",
		},
		{
			name:         "missing dataset format",
			oldValue:     "  format: jsonl\n",
			invalidValue: "",
			message:      "dataset.format",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "evaluation.yaml")
			content := strings.Replace(validEvaluationConfigForTest, test.oldValue, test.invalidValue, 1)
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

			_, err := loadEvaluationConfig(path)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.message)
		})
	}
}

func TestLoadEvaluationConfigRejectsUnknownField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evaluation.yaml")
	content := validEvaluationConfigForTest + "unsupported: true\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	_, err := loadEvaluationConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported")
}

func TestExpandEvaluationEnvironment(t *testing.T) {
	value, err := expandEvaluationEnvironment(
		"${FOUNDRY_MODEL_NAME}",
		[]string{"FOUNDRY_MODEL_NAME=gpt-5"},
	)
	require.NoError(t, err)
	assert.Equal(t, "gpt-5", value)

	_, err = expandEvaluationEnvironment("${MISSING}", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MISSING")
}

func TestEvaluatorDefaultThreshold(t *testing.T) {
	assert.Equal(t, new(3.0), evaluatorDefaultThreshold("builtin.relevance"))
	assert.Equal(t, new(0.5), evaluatorDefaultThreshold("builtin.f1_score"))
	assert.Nil(t, evaluatorDefaultThreshold("custom.evaluator"))
}
