// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package scaffold

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

const evaluationServiceHostForTest = "azure.ai.evaluation"

func TestProjectCreatesRunnableScaffold(t *testing.T) {
	target := t.TempDir()

	files, err := Project(target, Options{ProjectName: "sample-evaluation"})
	require.NoError(t, err)
	require.Len(t, files, len(projectFiles))

	azureYAML, err := os.ReadFile(filepath.Join(target, "azure.yaml")) //nolint:gosec
	require.NoError(t, err)
	assert.Contains(t, string(azureYAML), "name: sample-evaluation")
	assert.Contains(t, string(azureYAML), "host: azure.ai.evaluation")
	assert.Contains(t, string(azureYAML), "datasetName: sample-evaluation-dataset")

	var project struct {
		Name  string `yaml:"name"`
		Infra struct {
			Provider string `yaml:"provider"`
			Path     string `yaml:"path"`
		} `yaml:"infra"`
		Services map[string]struct {
			Host        string `yaml:"host"`
			Project     string `yaml:"project"`
			Script      string `yaml:"script"`
			Dataset     string `yaml:"dataset"`
			DatasetName string `yaml:"datasetName"`
		} `yaml:"services"`
	}
	require.NoError(t, yaml.Unmarshal(azureYAML, &project))
	assert.Equal(t, "sample-evaluation", project.Name)
	assert.Equal(t, "bicep", project.Infra.Provider)
	assert.Equal(t, "infra", project.Infra.Path)
	require.Contains(t, project.Services, "evaluation")
	assert.Equal(t, evaluationServiceHostForTest, project.Services["evaluation"].Host)
	assert.Equal(t, ".", project.Services["evaluation"].Project)
	assert.Equal(t, "src/evaluate.py", project.Services["evaluation"].Script)
	assert.Equal(t, "data/evaluation.jsonl", project.Services["evaluation"].Dataset)
	assert.Equal(t, "sample-evaluation-dataset", project.Services["evaluation"].DatasetName)

	python, err := os.ReadFile(filepath.Join(target, "src", "evaluate.py")) //nolint:gosec
	require.NoError(t, err)
	assert.Contains(t, string(python), "def run_local(")
	assert.Contains(t, string(python), "def run_remote(")

	envExample, err := os.ReadFile(filepath.Join(target, ".env.example")) //nolint:gosec
	require.NoError(t, err)
	assert.Contains(t, string(envExample), "FOUNDRY_PROJECT_ENDPOINT=")
	assert.Contains(t, string(envExample), "FOUNDRY_MODEL_NAME=")

	dataset, err := os.Open(filepath.Join(target, "data", "evaluation.jsonl")) //nolint:gosec
	require.NoError(t, err)
	defer dataset.Close()

	rows := 0
	scanner := bufio.NewScanner(dataset)
	for scanner.Scan() {
		if len(scanner.Bytes()) == 0 {
			continue
		}

		var item map[string]any
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &item))
		require.IsType(t, "", item["query"])
		require.IsType(t, "", item["ground_truth"])
		rows++
	}
	require.NoError(t, scanner.Err())
	require.Equal(t, 2, rows)
}

func TestProjectDoesNotOverwriteWithoutForce(t *testing.T) {
	target := t.TempDir()
	_, err := Project(target, Options{ProjectName: "sample-evaluation"})
	require.NoError(t, err)

	azurePath := filepath.Join(target, "azure.yaml")
	require.NoError(t, os.WriteFile(azurePath, []byte("user content\n"), 0o600))

	_, err = Project(target, Options{ProjectName: "sample-evaluation"})
	require.Error(t, err)
	require.True(t, IsConflict(err))

	content, readErr := os.ReadFile(azurePath) //nolint:gosec
	require.NoError(t, readErr)
	require.Equal(t, "user content\n", string(content))
}

func TestProjectForceOnlyOverwritesManagedFiles(t *testing.T) {
	target := t.TempDir()
	_, err := Project(target, Options{ProjectName: "first-evaluation"})
	require.NoError(t, err)

	unmanaged := filepath.Join(target, "notes.txt")
	require.NoError(t, os.WriteFile(unmanaged, []byte("keep\n"), 0o600))

	_, err = Project(target, Options{ProjectName: "second-evaluation", Force: true})
	require.NoError(t, err)

	azureYAML, err := os.ReadFile(filepath.Join(target, "azure.yaml")) //nolint:gosec
	require.NoError(t, err)
	assert.Contains(t, string(azureYAML), "name: second-evaluation")

	content, err := os.ReadFile(unmanaged) //nolint:gosec
	require.NoError(t, err)
	require.Equal(t, "keep\n", string(content))
}

func TestProjectRejectsSymlinkedManagedPaths(t *testing.T) {
	t.Run("target file", func(t *testing.T) {
		target := t.TempDir()
		outsideFile := filepath.Join(t.TempDir(), "outside.yaml")
		require.NoError(t, os.WriteFile(outsideFile, []byte("keep\n"), 0o600))
		if err := os.Symlink(outsideFile, filepath.Join(target, "azure.yaml")); err != nil {
			t.Skipf("symbolic links are unavailable: %v", err)
		}

		_, err := Project(target, Options{ProjectName: "sample-evaluation", Force: true})
		require.Error(t, err)
		require.Contains(t, err.Error(), "symbolic link")

		content, readErr := os.ReadFile(outsideFile) //nolint:gosec
		require.NoError(t, readErr)
		require.Equal(t, "keep\n", string(content))
	})

	t.Run("parent directory", func(t *testing.T) {
		target := t.TempDir()
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(target, "src")); err != nil {
			t.Skipf("symbolic links are unavailable: %v", err)
		}

		_, err := Project(target, Options{ProjectName: "sample-evaluation", Force: true})
		require.Error(t, err)
		require.Contains(t, err.Error(), "symbolic link")
		_, statErr := os.Stat(filepath.Join(outside, "evaluate.py"))
		require.ErrorIs(t, statErr, os.ErrNotExist)
	})
}

func TestValidateProjectName(t *testing.T) {
	for _, valid := range []string{"a", "model-evaluation", "eval123"} {
		require.NoError(t, ValidateProjectName(valid))
	}
	for _, invalid := range []string{"", "-eval", "eval-", "Eval", "eval_name", "../eval"} {
		require.Error(t, ValidateProjectName(invalid))
	}
}

func TestDefaultProjectName(t *testing.T) {
	assert.Equal(t, "my-evaluation", DefaultProjectName(filepath.Join("root", "My Evaluation")))
	assert.Equal(t, "model-evaluation", DefaultProjectName(filepath.Join("root", "___")))
}
