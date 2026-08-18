// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"testing"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/stretchr/testify/require"
)

func TestRootCommandHasExpectedCommands(t *testing.T) {
	root := NewRootCommand()
	for _, name := range []string{"init", "version", "listen", "metadata"} {
		command, _, err := root.Find([]string{name})
		require.NoError(t, err)
		require.Equal(t, name, command.Name())
	}
	require.NoError(t, azdext.ValidateNoReservedFlagConflicts(root))
}
