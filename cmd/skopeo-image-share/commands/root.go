package commands

import (
	"context"
	"fmt"

	"github.com/ngicks/skopeo-image-share/pkg/cli/ssh"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
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

var remoteTarget ssh.Target

func bindRemoteTargetFlags(f *pflag.FlagSet) {
	f.StringVar(&remoteTarget.Name, "remote-name", "", "ssh config destination name")
	f.StringVar(&remoteTarget.User, "remote-user", "", "remote ssh user (optional)")
	f.StringVar(&remoteTarget.Host, "remote-host", "", "remote ssh hostname/address")
	f.IntVar(&remoteTarget.Port, "remote-port", 0, "remote ssh port (0 uses ssh default/config)")
}

func validateRemoteTarget(t ssh.Target) error {
	if t.Name != "" {
		if t.User != "" || t.Host != "" || t.Port != 0 {
			return fmt.Errorf("--remote-name cannot be combined with --remote-user, --remote-host, or --remote-port")
		}
		return nil
	}
	if t.Host == "" {
		return fmt.Errorf("--remote-name or --remote-host is required")
	}
	if t.Port < 0 {
		return fmt.Errorf("--remote-port must be non-negative")
	}
	return nil
}
