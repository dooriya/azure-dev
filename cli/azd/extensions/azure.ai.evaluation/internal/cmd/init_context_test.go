// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFoundryProjectID(t *testing.T) {
	valid := "/subscriptions/sub/resourceGroups/rg/providers/" +
		"Microsoft.CognitiveServices/accounts/account/projects/project"

	resourceID, err := parseFoundryProjectID(valid)
	require.NoError(t, err)
	assert.Equal(t, "sub", resourceID.SubscriptionID)
	assert.Equal(t, "rg", resourceID.ResourceGroupName)
	assert.Equal(t, "project", resourceID.Name)

	_, err = parseFoundryProjectID(
		"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.CognitiveServices/accounts/account",
	)
	require.Error(t, err)
}

func TestEvaluationInitTargetEnvironmentValues(t *testing.T) {
	target := &evaluationInitTarget{
		ProjectID:       "project-id",
		ProjectEndpoint: "https://account.services.ai.azure.com/api/projects/project",
		ModelDeployment: "gpt-5-1",
		SubscriptionID:  "sub",
		TenantID:        "tenant",
		Location:        "northcentralus",
		ResourceGroup:   "rg",
		AccountName:     "account",
		ProjectName:     "project",
	}

	values := target.environmentValues()
	assert.Equal(t, target.ProjectEndpoint, values["FOUNDRY_PROJECT_ENDPOINT"])
	assert.Equal(t, target.ModelDeployment, values["FOUNDRY_MODEL_NAME"])
	assert.Equal(t, target.ProjectID, values["AZURE_AI_PROJECT_ID"])
	assert.Equal(t, "https://account.openai.azure.com/", values["AZURE_OPENAI_ENDPOINT"])
}

func TestDefaultEvaluationDeploymentName(t *testing.T) {
	assert.Equal(t, "gpt-4-1-mini", defaultEvaluationDeploymentName("gpt-4.1-mini"))
	assert.Equal(t, "evaluation-model", defaultEvaluationDeploymentName("..."))
}

func TestProjectModeOptionConflict(t *testing.T) {
	_, err := resolveEvaluationInitTarget(
		t.Context(),
		nil,
		&initFlags{newProject: true, projectID: "project"},
		true,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--new-project")
}

func TestNoPromptRequiresExplicitProjectMode(t *testing.T) {
	_, err := resolveEvaluationInitTarget(t.Context(), nil, &initFlags{}, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--project-id")
	assert.Contains(t, err.Error(), "--new-project")
}

func TestValidateEvaluationEnvironmentName(t *testing.T) {
	for _, name := range []string{"dev", "eval.dev", "eval_dev", "eval(test)"} {
		require.NoError(t, validateEvaluationEnvironmentName(name))
	}
	for _, name := range []string{"", ".", "..", `..\other`, "child/env"} {
		require.Error(t, validateEvaluationEnvironmentName(name))
	}
}

func TestRejectNestedAzdProject(t *testing.T) {
	parent := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(parent, "azure.yaml"), []byte("name: parent\n"), 0o600))
	child := filepath.Join(parent, "child")
	require.NoError(t, os.MkdirAll(child, 0o750))

	require.Error(t, rejectNestedAzdProject(child))
	require.NoError(t, rejectNestedAzdProject(parent))
}

func TestSupportsGenerativeEvaluation(t *testing.T) {
	assert.True(t, supportsGenerativeEvaluation([]string{"chatCompletion"}))
	assert.True(t, supportsGenerativeEvaluation([]string{"responses"}))
	assert.False(t, supportsGenerativeEvaluation([]string{"embeddings"}))
	assert.False(t, supportsGenerativeEvaluation([]string{"imageGenerations"}))
}
