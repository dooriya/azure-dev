// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJoinProjectPath(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "src"), 0o750))

	path, err := joinProjectPath(root, "src", "evaluate.py")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "src", "evaluate.py"), path)
}

func TestJoinProjectPathRejectsEscapes(t *testing.T) {
	root := t.TempDir()

	for _, value := range []string{"..", filepath.Join("src", "..", "..", "secret"), filepath.VolumeName(root) + `\secret`} {
		if value == `\secret` {
			continue
		}
		_, err := joinProjectPath(root, value)
		require.Error(t, err)
	}
}

func TestJoinProjectPathRejectsEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}

	_, err := joinProjectPath(root, "linked", "result.json")
	require.Error(t, err)
}
