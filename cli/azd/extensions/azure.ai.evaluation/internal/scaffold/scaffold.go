// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

// Package scaffold materializes the Evaluation extension's starter project.
package scaffold

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
)

//go:embed templates/**
var templateFS embed.FS

var projectNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

var (
	errConflict           = errors.New("scaffold files already exist")
	errInvalidProjectName = errors.New("invalid project name")
)

// Options controls project scaffolding.
type Options struct {
	ProjectName     string
	ProjectEndpoint string
	ModelDeployment string
	JudgeDeployment string
	Force           bool
}

// ExistingProject reports whether the scaffold targets an existing Foundry project.
func (o Options) ExistingProject() bool {
	return strings.TrimSpace(o.ProjectEndpoint) != ""
}

type fileDefinition struct {
	source   string
	target   string
	render   bool
	preserve bool
}

var commonProjectFiles = []fileDefinition{
	{source: "templates/gitignore", target: ".gitignore"},
	{source: "templates/env.example", target: ".env.example"},
	{source: "templates/README.md", target: "README.md"},
	{source: "templates/requirements.txt", target: "requirements.txt"},
	{source: "templates/evaluation.yaml.tmpl", target: "evaluation.yaml", render: true},
	{source: "templates/data/evaluation.jsonl", target: "data/evaluation.jsonl"},
	{source: "templates/src/evaluation_config.py", target: "src/evaluation_config.py"},
	{source: "templates/src/evaluation_runtime.py", target: "src/evaluation_runtime.py"},
	{source: "templates/src/evaluate.py", target: "src/evaluate.py"},
}

func projectFiles(options Options) []fileDefinition {
	files := make([]fileDefinition, 0, len(commonProjectFiles)+4)
	if options.ExistingProject() {
		files = append(files, fileDefinition{
			source: "templates/azure-existing.yaml.tmpl",
			target: "azure.yaml",
			render: true,
		})
	} else {
		files = append(files, fileDefinition{
			source: "templates/azure.yaml.tmpl",
			target: "azure.yaml",
			render: true,
		})
	}
	files = append(files, commonProjectFiles...)
	if options.ExistingProject() {
		files = append(files, fileDefinition{
			source:   "templates/env.tmpl",
			target:   ".env",
			render:   true,
			preserve: true,
		})
	}
	if !options.ExistingProject() {
		files = append(files,
			fileDefinition{source: "templates/infra/main.bicep", target: "infra/main.bicep"},
			fileDefinition{source: "templates/infra/resources.bicep", target: "infra/resources.bicep"},
			fileDefinition{source: "templates/infra/main.parameters.json", target: "infra/main.parameters.json"},
		)
	}
	return files
}

// Project creates a model evaluation project under target.
func Project(target string, options Options) ([]string, error) {
	if err := ValidateProjectName(options.ProjectName); err != nil {
		return nil, err
	}

	target, err := filepath.Abs(target)
	if err != nil {
		return nil, fmt.Errorf("resolving scaffold target: %w", err)
	}
	targetInfo, err := os.Lstat(target)
	if err != nil {
		return nil, fmt.Errorf("inspecting scaffold target: %w", err)
	}
	if targetInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("scaffold target %q must not be a symbolic link", target)
	}
	if !targetInfo.IsDir() {
		return nil, fmt.Errorf("scaffold target %q must be a directory", target)
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		return nil, fmt.Errorf("opening scaffold target: %w", err)
	}
	defer root.Close()

	files := projectFiles(options)
	rendered := make(map[string][]byte, len(files))
	preserved := make(map[string]bool)
	conflicts := make([]string, 0)
	for _, definition := range files {
		outputPath := filepath.Join(target, filepath.FromSlash(definition.target))
		relativePath := filepath.FromSlash(definition.target)
		if err := rejectSymlinkComponents(root, relativePath); err != nil {
			return nil, err
		}
		if _, statErr := root.Lstat(relativePath); statErr == nil {
			if definition.preserve {
				preserved[definition.target] = true
				continue
			}
			conflicts = append(conflicts, definition.target)
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return nil, fmt.Errorf("inspecting %s: %w", outputPath, statErr)
		}

		content, readErr := templateFS.ReadFile(definition.source)
		if readErr != nil {
			return nil, fmt.Errorf("reading embedded template %s: %w", definition.source, readErr)
		}
		if definition.render {
			content, readErr = renderTemplate(definition.source, content, options)
			if readErr != nil {
				return nil, readErr
			}
		}
		rendered[definition.target] = content
	}

	if len(conflicts) > 0 && !options.Force {
		return nil, fmt.Errorf("%w: %s", errConflict, strings.Join(conflicts, ", "))
	}

	created := make([]string, 0, len(files))
	for _, definition := range files {
		if preserved[definition.target] {
			continue
		}
		outputPath := filepath.Join(target, filepath.FromSlash(definition.target))
		relativePath := filepath.FromSlash(definition.target)
		if err := root.MkdirAll(filepath.Dir(relativePath), 0o750); err != nil {
			return created, fmt.Errorf("creating directory for %s: %w", outputPath, err)
		}
		if err := rejectSymlinkComponents(root, relativePath); err != nil {
			return created, err
		}
		// Generated project files are intentionally readable by standard tooling.
		if err := root.WriteFile(relativePath, rendered[definition.target], 0o644); err != nil { //nolint:gosec
			return created, fmt.Errorf("writing %s: %w", outputPath, err)
		}
		created = append(created, outputPath)
	}
	return created, nil
}

func rejectSymlinkComponents(root *os.Root, name string) error {
	current := ""
	for component := range strings.SplitSeq(filepath.Clean(name), string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := root.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspecting scaffold path %q: %w", name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("scaffold path %q contains symbolic link %q", name, current)
		}
	}
	return nil
}

func renderTemplate(name string, content []byte, options Options) ([]byte, error) {
	parsed, err := template.New(filepath.Base(name)).Option("missingkey=error").Parse(string(content))
	if err != nil {
		return nil, fmt.Errorf("parsing embedded template %s: %w", name, err)
	}
	var output bytes.Buffer
	if err := parsed.Execute(&output, options); err != nil {
		return nil, fmt.Errorf("rendering embedded template %s: %w", name, err)
	}
	return output.Bytes(), nil
}

// ValidateProjectName validates an azd project name used by the scaffold.
func ValidateProjectName(name string) error {
	if !projectNamePattern.MatchString(name) {
		return fmt.Errorf("%w %q", errInvalidProjectName, name)
	}
	return nil
}

// DefaultProjectName derives a valid project name from a directory.
func DefaultProjectName(target string) string {
	name := strings.ToLower(filepath.Base(filepath.Clean(target)))
	var normalized strings.Builder
	lastHyphen := false
	for _, value := range name {
		valid := value >= 'a' && value <= 'z' || value >= '0' && value <= '9'
		if valid {
			normalized.WriteRune(value)
			lastHyphen = false
			continue
		}
		if normalized.Len() > 0 && !lastHyphen {
			normalized.WriteByte('-')
			lastHyphen = true
		}
	}
	result := strings.Trim(normalized.String(), "-")
	if len(result) > 64 {
		result = strings.TrimRight(result[:64], "-")
	}
	if result == "" {
		return "model-evaluation"
	}
	return result
}

// IsConflict reports whether an error represents existing scaffold-managed files.
func IsConflict(err error) bool {
	return errors.Is(err, errConflict)
}

// IsInvalidProjectName reports whether an error represents an invalid project name.
func IsInvalidProjectName(err error) bool {
	return errors.Is(err, errInvalidProjectName)
}
