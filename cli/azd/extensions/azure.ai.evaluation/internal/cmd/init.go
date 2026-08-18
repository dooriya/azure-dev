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
	name            string
	force           bool
	newProject      bool
	projectID       string
	modelDeployment string
	judgeDeployment string
	subscriptionID  string
	location        string
}

func newInitCommand(extCtx *azdext.ExtensionContext) *cobra.Command {
	flags := &initFlags{}
	command := &cobra.Command{
		Use:   "init",
		Short: "Scaffold a model evaluation project.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := azdext.WithAccessToken(cmd.Context())
			target, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("determining the target directory: %w", err)
			}

			name := flags.name
			if name == "" {
				name = scaffold.DefaultProjectName(target)
			}
			if err := rejectNestedAzdProject(target); err != nil {
				return err
			}

			client, err := azdext.NewAzdClient()
			if err != nil {
				return fmt.Errorf("creating azd client: %w", err)
			}
			defer client.Close()

			initTarget, err := resolveEvaluationInitTarget(
				ctx,
				client,
				flags,
				extCtx != nil && extCtx.NoPrompt,
			)
			if err != nil {
				return err
			}

			files, err := scaffold.Project(target, scaffold.Options{
				ProjectName:     name,
				ProjectEndpoint: initTarget.ProjectEndpoint,
				ModelDeployment: initTarget.ModelDeployment,
				JudgeDeployment: initTarget.JudgeDeployment,
				Force:           flags.force,
			})
			if err != nil {
				return classifyScaffoldError(err)
			}

			envName := name
			if extCtx != nil && extCtx.Environment != "" {
				envName = extCtx.Environment
			}
			if err := validateEvaluationEnvironmentName(envName); err != nil {
				return err
			}
			environment, err := ensureEvaluationEnvironment(ctx, client, envName)
			if err != nil {
				return err
			}
			if err := persistEvaluationInitTarget(ctx, client, environment.GetName(), initTarget); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Initialized model evaluation project %q.\n", name)
			fmt.Fprintf(cmd.OutOrStdout(), "Created %d files in %s.\n\n", len(files), target)
			printEvaluationNextSteps(cmd, initTarget.ExistingProject())
			return nil
		},
	}

	command.Flags().StringVar(&flags.name, "name", "", "Project name written to azure.yaml")
	command.Flags().BoolVar(&flags.force, "force", false, "Overwrite files managed by the scaffold")
	command.Flags().BoolVar(
		&flags.newProject,
		"new-project",
		false,
		"Create a new Foundry project instead of selecting an existing project",
	)
	command.Flags().StringVar(
		&flags.projectID,
		"project-id",
		"",
		"ARM resource ID of an existing Foundry project",
	)
	command.Flags().StringVar(
		&flags.modelDeployment,
		"model-deployment",
		"",
		"Target model deployment that generates responses to evaluate",
	)
	command.Flags().StringVar(
		&flags.judgeDeployment,
		"judge-model-deployment",
		"",
		"Judge model deployment that scores responses with AI-assisted evaluators (defaults to target)",
	)
	command.Flags().StringVarP(&flags.subscriptionID, "subscription", "s", "", "Azure subscription ID")
	command.Flags().StringVarP(&flags.location, "location", "l", "", "Azure location for new resources")
	return command
}

func printEvaluationNextSteps(cmd *cobra.Command, existingProject bool) {
	fmt.Fprintln(cmd.OutOrStdout(), "Next steps:")
	fmt.Fprintln(cmd.OutOrStdout(), "  Run in Microsoft Foundry:")
	fmt.Fprintln(cmd.OutOrStdout(), "    1. Run: azd provision")
	fmt.Fprintln(cmd.OutOrStdout(), "    2. Run: azd deploy")
	fmt.Fprintln(cmd.OutOrStdout(), "  Run locally (optional):")
	if existingProject {
		fmt.Fprintln(cmd.OutOrStdout(), "    1. Run: python -m venv .venv")
		fmt.Fprintln(cmd.OutOrStdout(), "    2. Activate .venv and run: python -m pip install -r requirements.txt")
		fmt.Fprintln(cmd.OutOrStdout(), "    3. Run: python src/evaluate.py --local")
		return
	}
	fmt.Fprintln(cmd.OutOrStdout(), "    1. After provisioning, run: azd env get-values > .env")
	fmt.Fprintln(cmd.OutOrStdout(), "    2. Run: python -m venv .venv")
	fmt.Fprintln(cmd.OutOrStdout(), "    3. Activate .venv and run: python -m pip install -r requirements.txt")
	fmt.Fprintln(cmd.OutOrStdout(), "    4. Run: python src/evaluate.py --local")
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
