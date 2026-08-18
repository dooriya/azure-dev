// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/spf13/cobra"
)

const evaluationServiceHost = "azure.ai.evaluation"

// NewRootCommand creates the Evaluation extension command tree.
func NewRootCommand() *cobra.Command {
	rootCmd, extCtx := azdext.NewExtensionRootCommand(azdext.ExtensionCommandOptions{
		Name:  "evaluation",
		Use:   "evaluation <command> [options]",
		Short: "Run model evaluations locally or with Microsoft Foundry. (Preview)",
	})
	rootCmd.SilenceUsage = true
	rootCmd.SilenceErrors = true
	rootCmd.CompletionOptions.DisableDefaultCmd = true
	rootCmd.SetHelpCommand(&cobra.Command{Hidden: true})

	rootCmd.AddCommand(newInitCommand(extCtx))
	rootCmd.AddCommand(newVersionCommand())
	rootCmd.AddCommand(azdext.NewListenCommand(configureExtensionHost))
	rootCmd.AddCommand(azdext.NewMetadataCommand("1.0", "azure.ai.evaluation", func() *cobra.Command {
		return rootCmd
	}))

	return rootCmd
}

func configureExtensionHost(host *azdext.ExtensionHost) {
	azdClient := host.Client()
	host.WithServiceTarget(evaluationServiceHost, func() azdext.ServiceTargetProvider {
		return newEvaluationServiceTarget(azdClient)
	})
}
