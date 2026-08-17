// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/spf13/cobra"
)

func newProvisionCommand(extCtx *azdext.ExtensionContext) *cobra.Command {
	var preview bool
	command := &cobra.Command{
		Use:   "provision",
		Short: "Provision the Foundry project and evaluation resources.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			workflowArgs := []string{"provision"}
			if preview {
				workflowArgs = append(workflowArgs, "--preview")
			}
			return runAzdCommand(
				cmd.Context(),
				workflowArgs,
				extCtx,
				cmd.OutOrStdout(),
				cmd.ErrOrStderr(),
			)
		},
	}
	command.Flags().BoolVar(&preview, "preview", false, "Preview infrastructure changes without applying them")
	return command
}

func newDeployCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "deploy",
		Short: "Run the evaluation as a managed Microsoft Foundry job.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return deployEvaluationServices(
				azdext.WithAccessToken(cmd.Context()),
				cmd,
			)
		},
	}
}

func deployEvaluationServices(ctx context.Context, command *cobra.Command) error {
	client, err := azdext.NewAzdClient()
	if err != nil {
		return fmt.Errorf("creating azd client: %w", err)
	}
	defer client.Close()

	response, err := client.Project().Get(ctx, &azdext.EmptyRequest{})
	if err != nil {
		return &azdext.LocalError{
			Message:    fmt.Sprintf("Failed to load the azd project: %s", err),
			Code:       "evaluation_project_not_found",
			Category:   azdext.LocalErrorCategoryDependency,
			Suggestion: "Run 'azd ai evaluation init' from an evaluation project directory.",
		}
	}
	if response.GetProject() == nil {
		return &azdext.LocalError{
			Message:    "The azd project configuration is missing.",
			Code:       "evaluation_project_not_found",
			Category:   azdext.LocalErrorCategoryDependency,
			Suggestion: "Run 'azd ai evaluation init' from an evaluation project directory.",
		}
	}

	services := response.GetProject().GetServices()
	names := slices.Sorted(maps.Keys(services))
	evaluationNames := make([]string, 0, len(names))
	for _, name := range names {
		if services[name].GetHost() == evaluationServiceHost {
			evaluationNames = append(evaluationNames, name)
		}
	}
	if len(evaluationNames) == 0 {
		return &azdext.LocalError{
			Message:    "The azd project does not define an azure.ai.evaluation service.",
			Code:       "evaluation_service_not_found",
			Category:   azdext.LocalErrorCategoryValidation,
			Suggestion: "Run 'azd ai evaluation init', or add an azure.ai.evaluation service to azure.yaml.",
		}
	}

	target := newEvaluationServiceTarget(client)
	for _, name := range evaluationNames {
		service := services[name]
		progress := func(message string) {
			fmt.Fprintf(command.OutOrStdout(), "%s: %s\n", name, message)
		}

		serviceContext := &azdext.ServiceContext{}
		packageResult, err := target.Package(ctx, service, serviceContext, progress)
		if err != nil {
			return err
		}
		serviceContext.Package = packageResult.GetArtifacts()

		publishResult, err := target.Publish(
			ctx,
			service,
			serviceContext,
			&azdext.TargetResource{},
			&azdext.PublishOptions{},
			progress,
		)
		if err != nil {
			return err
		}
		serviceContext.Publish = publishResult.GetArtifacts()

		if _, err := target.Deploy(
			ctx,
			service,
			serviceContext,
			&azdext.TargetResource{},
			progress,
		); err != nil {
			return err
		}
	}
	return nil
}

func runAzdCommand(
	ctx context.Context,
	args []string,
	extCtx *azdext.ExtensionContext,
	stdout io.Writer,
	stderr io.Writer,
) error {
	if len(args) == 0 {
		return fmt.Errorf("azd command is empty")
	}
	commandName := args[0]
	args = slices.Clone(args)
	if extCtx != nil && extCtx.Environment != "" {
		args = append(args, "--environment", extCtx.Environment)
	}
	if extCtx != nil && extCtx.NoPrompt {
		args = append(args, "--no-prompt")
	}

	executable, err := exec.LookPath("azd")
	if err != nil {
		return &azdext.LocalError{
			Message:    "Could not find azd on PATH.",
			Code:       "evaluation_azd_not_found",
			Category:   azdext.LocalErrorCategoryDependency,
			Suggestion: "Install Azure Developer CLI and retry.",
		}
	}

	// A separate azd host is required because the host running this custom
	// command cannot re-enter the same extension as a service-target listener.
	process := exec.CommandContext(ctx, executable, args...) //nolint:gosec
	process.Stdout = stdout
	process.Stderr = stderr
	process.Env = childAzdEnvironment(os.Environ())
	if extCtx != nil && extCtx.Cwd != "" {
		process.Dir = extCtx.Cwd
	}
	if err := process.Run(); err != nil {
		return fmt.Errorf("running azd %s: %w", commandName, err)
	}
	return nil
}

func childAzdEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		key, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		switch strings.ToUpper(key) {
		case "AZD_SERVER", "AZD_ACCESS_TOKEN":
			continue
		default:
			result = append(result, entry)
		}
	}
	return result
}
