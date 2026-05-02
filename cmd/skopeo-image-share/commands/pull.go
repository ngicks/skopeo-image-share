package commands

import (
	"fmt"

	"github.com/ngicks/skopeo-image-share/pkg/cli/skopeo"
	"github.com/ngicks/skopeo-image-share/pkg/skopeoimageshare"
	"github.com/spf13/cobra"
)

var pullCmd = &cobra.Command{
	Use:   "pull IMAGE [IMAGE...]",
	Short: "Pull images from remote to local.",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runPull,
}

var pullFlags struct {
	localTransport  string
	localPath       string
	remoteTransport string
	remotePath      string
	localDumpDir    string
	jobs            int
	dryRun          bool
	assumeLocalHas  []string
	keepGoing       bool
}

func init() {
	rootCmd.AddCommand(pullCmd)

	f := pullCmd.Flags()
	f.StringVar(&pullFlags.localTransport, "local-transport", "containers-storage", "containers-storage|docker-daemon|oci")
	f.StringVar(&pullFlags.localPath, "local-path", "", "local oci: dir (only when --local-transport=oci)")
	bindRemoteTargetFlags(f)
	f.StringVar(&pullFlags.remoteTransport, "remote-transport", "containers-storage", "containers-storage|docker-daemon|oci")
	f.StringVar(&pullFlags.remotePath, "remote-path", "", "remote oci: dir (only when --remote-transport=oci)")
	f.StringVar(&pullFlags.localDumpDir, "local-dumpdir", "",
		"base of the local on-disk store layout; "+
			"when empty, falls back to ${XDG_DATA_HOME:-$HOME/.local/share}/skopeo-image-share")
	f.IntVar(&pullFlags.jobs, "jobs", 4, "per-blob parallelism")
	f.BoolVar(&pullFlags.dryRun, "dry-run", false, "no mutation; emit a plan instead")
	f.StringSliceVar(&pullFlags.assumeLocalHas, "assume-local-has", nil, "raw blob digests local already has (skips enumeration)")
	f.BoolVar(&pullFlags.keepGoing, "keep-going", false, "continue on per-image failure")
}

func runPull(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	share, err := initShare(ctx,
		skopeoimageshare.LocalConfig{
			BaseDir:   pullFlags.localDumpDir,
			Transport: skopeo.Transport(pullFlags.localTransport),
			OCIPath:   pullFlags.localPath,
		},
		skopeoimageshare.RemoteConfig{
			Transport: skopeo.Transport(pullFlags.remoteTransport),
			OCIPath:   pullFlags.remotePath,
		},
	)
	if err != nil {
		return err
	}
	defer share.Close()

	res, err := share.Pull(ctx, skopeoimageshare.PullArgs{
		Images:         args,
		Jobs:           pullFlags.jobs,
		DryRun:         pullFlags.dryRun,
		AssumeLocalHas: pullFlags.assumeLocalHas,
		KeepGoing:      pullFlags.keepGoing,
	})
	for _, r := range res.Reports {
		fmt.Fprintln(cmd.OutOrStdout(), r.SummaryLine())
	}
	if err != nil {
		return err
	}
	if res.FailedCount > 0 {
		return fmt.Errorf("%d image(s) failed", res.FailedCount)
	}
	return nil
}
