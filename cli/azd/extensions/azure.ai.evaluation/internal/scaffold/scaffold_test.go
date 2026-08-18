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
	options := Options{ProjectName: "sample-evaluation"}

	files, err := Project(target, options)
	require.NoError(t, err)
	require.Len(t, files, len(projectFiles(options)))

	azureYAML, err := os.ReadFile(filepath.Join(target, "azure.yaml")) //nolint:gosec
	require.NoError(t, err)
	assert.Contains(t, string(azureYAML), "name: sample-evaluation")
	assert.Contains(t, string(azureYAML), "host: azure.ai.evaluation")
	assert.Contains(t, string(azureYAML), "configFile: evaluation.yaml")

	var project struct {
		Name  string `yaml:"name"`
		Infra struct {
			Provider string `yaml:"provider"`
			Path     string `yaml:"path"`
		} `yaml:"infra"`
		Services map[string]struct {
			Host       string `yaml:"host"`
			Project    string `yaml:"project"`
			ConfigFile string `yaml:"configFile"`
		} `yaml:"services"`
	}
	require.NoError(t, yaml.Unmarshal(azureYAML, &project))
	assert.Equal(t, "sample-evaluation", project.Name)
	assert.Equal(t, "bicep", project.Infra.Provider)
	assert.Equal(t, "infra", project.Infra.Path)
	require.Contains(t, project.Services, "evaluation")
	assert.Equal(t, evaluationServiceHostForTest, project.Services["evaluation"].Host)
	assert.Equal(t, ".", project.Services["evaluation"].Project)
	assert.Equal(t, "evaluation.yaml", project.Services["evaluation"].ConfigFile)

	evaluationYAML, err := os.ReadFile(filepath.Join(target, "evaluation.yaml")) //nolint:gosec
	require.NoError(t, err)
	assert.Contains(t, string(evaluationYAML), "profile: starter")
	assert.Contains(t, string(evaluationYAML), "FOUNDRY_JUDGE_MODEL_NAME")
	assert.Contains(t, string(evaluationYAML), "minPassRate: 0.8")

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
	require.Equal(t, 4, rows)
}

func TestProjectCreatesExistingProjectScaffold(t *testing.T) {
	target := t.TempDir()
	options := Options{
		ProjectName:     "existing-evaluation",
		ProjectEndpoint: "https://account.services.ai.azure.com/api/projects/project",
		ModelDeployment: "gpt-5-1",
		JudgeDeployment: "gpt-5-mini",
	}

	files, err := Project(target, options)
	require.NoError(t, err)
	require.Len(t, files, len(projectFiles(options)))

	azureYAML, err := os.ReadFile(filepath.Join(target, "azure.yaml")) //nolint:gosec
	require.NoError(t, err)
	var project struct {
		Infra struct {
			Provider string `yaml:"provider"`
			Path     string `yaml:"path"`
		} `yaml:"infra"`
		Services map[string]struct {
			Host     string   `yaml:"host"`
			Endpoint string   `yaml:"endpoint"`
			Uses     []string `yaml:"uses"`
		} `yaml:"services"`
	}
	require.NoError(t, yaml.Unmarshal(azureYAML, &project))
	assert.Equal(t, "microsoft.foundry", project.Infra.Provider)
	assert.Empty(t, project.Infra.Path)
	assert.Equal(t, "azure.ai.project", project.Services["ai-project"].Host)
	assert.Equal(t, options.ProjectEndpoint, project.Services["ai-project"].Endpoint)
	assert.Equal(t, []string{"ai-project"}, project.Services["evaluation"].Uses)

	envExample, err := os.ReadFile(filepath.Join(target, ".env.example")) //nolint:gosec
	require.NoError(t, err)
	assert.Contains(t, string(envExample), "FOUNDRY_PROJECT_ENDPOINT=https://<account-name>")
	assert.Contains(t, string(envExample), "FOUNDRY_MODEL_NAME=<model-deployment-name>")
	assert.NotContains(t, string(envExample), options.ProjectEndpoint)

	localEnv, err := os.ReadFile(filepath.Join(target, ".env")) //nolint:gosec
	require.NoError(t, err)
	assert.Contains(t, string(localEnv), "FOUNDRY_PROJECT_ENDPOINT="+options.ProjectEndpoint)
	assert.Contains(t, string(localEnv), "FOUNDRY_MODEL_NAME="+options.ModelDeployment)
	assert.Contains(t, string(localEnv), "FOUNDRY_JUDGE_MODEL_NAME="+options.JudgeDeployment)

	_, err = os.Stat(filepath.Join(target, "infra", "main.bicep"))
	require.ErrorIs(t, err, os.ErrNotExist)
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

func TestProjectForcePreservesExistingLocalEnv(t *testing.T) {
	target := t.TempDir()
	options := Options{
		ProjectName:     "existing-evaluation",
		ProjectEndpoint: "https://account.services.ai.azure.com/api/projects/project",
		ModelDeployment: "gpt-5-1",
	}
	_, err := Project(target, options)
	require.NoError(t, err)

	localEnv := filepath.Join(target, ".env")
	require.NoError(t, os.WriteFile(localEnv, []byte("CUSTOM=value\n"), 0o600))
	options.ModelDeployment = "different-deployment"
	options.Force = true
	_, err = Project(target, options)
	require.NoError(t, err)

	content, err := os.ReadFile(localEnv) //nolint:gosec
	require.NoError(t, err)
	require.Equal(t, "CUSTOM=value\n", string(content))
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
