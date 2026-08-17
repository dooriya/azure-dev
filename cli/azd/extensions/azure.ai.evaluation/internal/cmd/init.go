// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"fmt"
	"os"

	"azure.ai.evaluation/internal/scaffold"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/spf13/cobra"
)

type initFlags struct {
	name  string
	force bool
}

func newInitCommand() *cobra.Command {
	flags := &initFlags{}
	command := &cobra.Command{
		Use:   "init",
		Short: "Scaffold a model evaluation project.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("determining the target directory: %w", err)
			}

			name := flags.name
			if name == "" {
				name = scaffold.DefaultProjectName(target)
			}

			files, err := scaffold.Project(target, scaffold.Options{
				ProjectName: name,
				Force:       flags.force,
			})
			if err != nil {
				return classifyScaffoldError(err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Initialized model evaluation project %q.\n", name)
			fmt.Fprintf(cmd.OutOrStdout(), "Created %d files in %s.\n\n", len(files), target)
			fmt.Fprintln(cmd.OutOrStdout(), "Next steps:")
			fmt.Fprintln(cmd.OutOrStdout(), "  1. Create a Python virtual environment and install requirements.txt.")
			fmt.Fprintln(cmd.OutOrStdout(), "  2. Copy .env.example to .env and set the project endpoint and model.")
			fmt.Fprintln(cmd.OutOrStdout(), "  3. Run: python src/evaluate.py --local")
			fmt.Fprintln(cmd.OutOrStdout(), "  4. Run: azd ai evaluation provision")
			fmt.Fprintln(cmd.OutOrStdout(), "  5. Run: azd ai evaluation deploy")
			return nil
		},
	}

	command.Flags().StringVar(&flags.name, "name", "", "Project name written to azure.yaml")
	command.Flags().BoolVar(&flags.force, "force", false, "Overwrite files managed by the scaffold")
	return command
}

func classifyScaffoldError(err error) error {
	if scaffold.IsConflict(err) {
		return &azdext.LocalError{
			Message:    err.Error(),
			Code:       "evaluation_scaffold_conflict",
			Category:   azdext.LocalErrorCategoryUser,
			Suggestion: "Use --force to overwrite scaffold-managed files, or choose an empty directory.",
		}
	}
	if scaffold.IsInvalidProjectName(err) {
		return &azdext.LocalError{
			Message:    err.Error(),
			Code:       "evaluation_invalid_project_name",
			Category:   azdext.LocalErrorCategoryValidation,
			Suggestion: "Use 1-64 lowercase letters, numbers, and hyphens; begin and end with a letter or number.",
		}
	}
	return fmt.Errorf("scaffolding evaluation project: %w", err)
}
