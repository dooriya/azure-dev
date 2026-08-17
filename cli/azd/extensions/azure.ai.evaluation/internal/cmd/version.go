// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"fmt"

	"azure.ai.evaluation/internal/version"

	"github.com/spf13/cobra"
)

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the Evaluation extension version.",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintf(
				cmd.OutOrStdout(),
				"Version: %s\nCommit: %s\nBuild Date: %s\n",
				version.Version,
				version.Commit,
				version.BuildDate,
			)
		},
	}
}
