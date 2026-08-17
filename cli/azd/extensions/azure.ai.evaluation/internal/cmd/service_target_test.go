// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestParseEvaluationServiceConfigDefaults(t *testing.T) {
	config, err := parseEvaluationServiceConfig(&azdext.ServiceConfig{Name: "evaluation"})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("src", "evaluate.py"), config.Script)
	assert.Equal(t, filepath.Join("data", "evaluation.jsonl"), config.Dataset)
	assert.Equal(t, "auto", config.DatasetVersion)
	assert.Equal(t, 3600, config.TimeoutSeconds)
	require.NotNil(t, config.Tracing)
	assert.True(t, *config.Tracing)
}

func TestParseEvaluationServiceConfigOverrides(t *testing.T) {
	properties, err := structpb.NewStruct(map[string]any{
		"script":         "evaluate.py",
		"dataset":        "input.jsonl",
		"results":        "output",
		"datasetName":    "sample",
		"datasetVersion": "2",
		"python":         "custom-python",
		"timeoutSeconds": 42,
		"noWait":         true,
		"tracing":        false,
		"captureContent": true,
	})
	require.NoError(t, err)

	config, err := parseEvaluationServiceConfig(&azdext.ServiceConfig{
		Name:                 "evaluation",
		AdditionalProperties: properties,
	})
	require.NoError(t, err)
	assert.Equal(t, "evaluate.py", config.Script)
	assert.Equal(t, "input.jsonl", config.Dataset)
	assert.Equal(t, "output", config.Results)
	assert.Equal(t, "sample", config.DatasetName)
	assert.Equal(t, "2", config.DatasetVersion)
	assert.Equal(t, "custom-python", config.Python)
	assert.Equal(t, 42, config.TimeoutSeconds)
	assert.True(t, config.NoWait)
	require.NotNil(t, config.Tracing)
	assert.False(t, *config.Tracing)
	assert.True(t, config.CaptureContent)
}

func TestParseEvaluationServiceConfigRejectsInvalidValues(t *testing.T) {
	properties, err := structpb.NewStruct(map[string]any{"timeoutSeconds": 0})
	require.NoError(t, err)

	_, err = parseEvaluationServiceConfig(&azdext.ServiceConfig{
		Name:                 "evaluation",
		AdditionalProperties: properties,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timeoutSeconds")
}

func TestRunEvaluationBuildsRemoteCommand(t *testing.T) {
	root := t.TempDir()
	pythonName := "go"
	if _, err := resolvePython(root, pythonName); err != nil {
		t.Skipf("test executable unavailable: %v", err)
	}

	runner := &recordingProcessRunner{}
	target := &evaluationServiceTarget{runner: runner}
	tracing := true
	files := &resolvedServiceFiles{
		config: evaluationServiceConfig{
			Python:         pythonName,
			DatasetName:    "sample",
			DatasetVersion: "3",
			TimeoutSeconds: 90,
			Tracing:        &tracing,
		},
		serviceRoot: root,
		script:      filepath.Join(root, "src", "evaluate.py"),
		dataset:     filepath.Join(root, "data", "evaluation.jsonl"),
		results:     filepath.Join(root, "results"),
	}

	err := target.runEvaluation(t.Context(), &azdext.ServiceConfig{
		Environment: map[string]string{
			"FOUNDRY_PROJECT_ENDPOINT": "https://example.services.ai.azure.com/api/projects/sample",
			"FOUNDRY_MODEL_NAME":       "model",
		},
	}, files)
	require.NoError(t, err)
	require.Len(t, runner.specs, 1)

	spec := runner.specs[0]
	assert.Equal(t, root, spec.directory)
	assert.Contains(t, spec.args, "--remote")
	assert.Contains(t, spec.args, "--dataset-name")
	assert.Contains(t, spec.args, "sample")
	assert.Contains(t, spec.args, "--dataset-version")
	assert.Contains(t, spec.args, "3")
	assert.Equal(t, "model", environmentValue(spec.environment, "FOUNDRY_MODEL_NAME"))
}

func TestRunEvaluationRequiresEndpointAndModel(t *testing.T) {
	root := t.TempDir()
	pythonName := "go"
	if _, err := resolvePython(root, pythonName); err != nil {
		t.Skipf("test executable unavailable: %v", err)
	}
	target := &evaluationServiceTarget{runner: &recordingProcessRunner{}}
	files := &resolvedServiceFiles{
		config: evaluationServiceConfig{
			Python:         pythonName,
			DatasetName:    "sample",
			DatasetVersion: "1",
			TimeoutSeconds: 90,
		},
		serviceRoot: root,
		script:      filepath.Join(root, "evaluate.py"),
		dataset:     filepath.Join(root, "evaluation.jsonl"),
		results:     filepath.Join(root, "results"),
	}

	t.Setenv("FOUNDRY_PROJECT_ENDPOINT", "")
	t.Setenv("AZURE_AI_PROJECT_ENDPOINT", "")
	t.Setenv("FOUNDRY_MODEL_NAME", "")
	t.Setenv("AZURE_AI_MODEL_DEPLOYMENT_NAME", "")

	err := target.runEvaluation(t.Context(), &azdext.ServiceConfig{}, files)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "endpoint")
}

func TestResolvePythonPrefersExplicitConfiguration(t *testing.T) {
	root := t.TempDir()
	venvPython := filepath.Join(root, ".venv", "bin", "python")
	require.NoError(t, os.MkdirAll(filepath.Dir(venvPython), 0o750))
	require.NoError(t, os.WriteFile(venvPython, []byte("stale"), 0o600))

	explicit, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("test executable unavailable: %v", err)
	}
	resolved, err := resolvePython(root, "go")
	require.NoError(t, err)
	assert.Equal(t, explicit, resolved)
}

func TestMergeEnvironmentSkipsEmptyOverrides(t *testing.T) {
	environment := mergeEnvironment(
		[]string{"FOUNDRY_MODEL_NAME=existing", "OTHER=value"},
		map[string]string{"FOUNDRY_MODEL_NAME": "", "NEW": "new-value"},
	)
	assert.Equal(t, "existing", environmentValue(environment, "FOUNDRY_MODEL_NAME"))
	assert.Equal(t, "new-value", environmentValue(environment, "NEW"))
}

type recordingProcessRunner struct {
	specs []processSpec
	err   error
}

func (r *recordingProcessRunner) Run(ctx context.Context, spec processSpec) error {
	r.specs = append(r.specs, spec)
	return r.err
}
