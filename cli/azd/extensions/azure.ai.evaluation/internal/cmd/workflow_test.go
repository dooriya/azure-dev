// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestChildAzdEnvironmentRemovesParentExtensionCredentials(t *testing.T) {
	environment := childAzdEnvironment([]string{
		"AZD_SERVER=localhost:1234",
		"AZD_ACCESS_TOKEN=secret",
		"FOUNDRY_PROJECT_ENDPOINT=https://example.test",
	})

	assert.Empty(t, environmentValue(environment, "AZD_SERVER"))
	assert.Empty(t, environmentValue(environment, "AZD_ACCESS_TOKEN"))
	assert.Equal(t, "https://example.test", environmentValue(environment, "FOUNDRY_PROJECT_ENDPOINT"))
}
