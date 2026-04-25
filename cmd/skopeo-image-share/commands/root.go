package commands

import (
	"context"

	"github.com/spf13/cobra"
)

func Execute(ctx context.Context) error {
	return rootCmd.ExecuteContext(ctx)
}

var rootCmd = &cobra.Command{
	Use:           "skopeo-image-share",
	Short:         "Share OCI images between two hosts efficiently over SSH using skopeo + sftp.",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args:          cobra.NoArgs,
	RunE:          runRoot,
}

func runRoot(cmd *cobra.Command, args []string) error {
	return cmd.Help()
}

// TODO: you may add initialization logic for root internal service construct here.
