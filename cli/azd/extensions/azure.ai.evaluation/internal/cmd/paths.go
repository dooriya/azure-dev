// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func joinProjectPath(projectRoot string, parts ...string) (string, error) {
	if strings.TrimSpace(projectRoot) == "" {
		return "", errors.New("project root is empty")
	}

	root, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", fmt.Errorf("resolving project root: %w", err)
	}
	root = filepath.Clean(root)

	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return "", errors.New("project-relative path is empty")
		}
		if filepath.IsAbs(part) || filepath.VolumeName(part) != "" {
			return "", fmt.Errorf("project-relative path %q must not be absolute", part)
		}
		for segment := range strings.SplitSeq(filepath.ToSlash(part), "/") {
			if segment == ".." {
				return "", fmt.Errorf("project-relative path %q must not contain '..'", part)
			}
		}
	}

	target, err := filepath.Abs(filepath.Join(append([]string{root}, parts...)...))
	if err != nil {
		return "", fmt.Errorf("resolving project-relative path: %w", err)
	}
	target = filepath.Clean(target)

	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes project root")
	}

	existing := target
	for {
		if _, statErr := os.Lstat(existing); statErr == nil {
			break
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", fmt.Errorf("inspecting project-relative path: %w", statErr)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", errors.New("project-relative path has no existing ancestor")
		}
		existing = parent
	}

	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolving project root symlinks: %w", err)
	}
	realExisting, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", fmt.Errorf("resolving project-relative symlinks: %w", err)
	}
	relative, err = filepath.Rel(realRoot, realExisting)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes project root through a symbolic link")
	}

	return target, nil
}
