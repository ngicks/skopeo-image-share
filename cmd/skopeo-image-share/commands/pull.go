package commands

import (
	"fmt"
	"time"

	"github.com/ngicks/skopeo-image-share/pkg/cli/ssh"
	"github.com/ngicks/skopeo-image-share/pkg/skopeoimageshare"
	"github.com/spf13/cobra"
)

var pullCmd = &cobra.Command{
	Use:   "pull",
	Short: "Pull images from remote to local.",
	Args:  cobra.ArbitraryArgs,
	RunE:  runPull,
}

var pullFlags struct {
	images          []string
	localTransport  string
	localPath       string
	remoteTransport string
	remotePath      string
	dataDir         string
	jobs            int
	dryRun          bool
	assumeLocalHas  []string
	keepGoing       bool
	retries         int
	retryMaxDelay   time.Duration
}

func init() {
	rootCmd.AddCommand(pullCmd)

	f := pullCmd.Flags()
	f.StringSliceVar(&pullFlags.images, "image", nil, "image ref to pull (repeatable)")
	f.StringVar(&pullFlags.localTransport, "local-transport", "containers-storage", "containers-storage|docker-daemon|oci")
	f.StringVar(&pullFlags.localPath, "local-path", "", "local oci: dir (only when --local-transport=oci)")
	bindRemoteTargetFlags(f)
	f.StringVar(&pullFlags.remoteTransport, "remote-transport", "containers-storage", "containers-storage|docker-daemon|oci")
	f.StringVar(&pullFlags.remotePath, "remote-path", "", "remote oci: dir (only when --remote-transport=oci)")
	f.StringVar(&pullFlags.dataDir, "data-dir", "", "override $XDG_DATA_HOME data dir")
	f.IntVar(&pullFlags.jobs, "jobs", 4, "per-blob parallelism")
	f.BoolVar(&pullFlags.dryRun, "dry-run", false, "no mutation; emit a plan instead")
	f.StringSliceVar(&pullFlags.assumeLocalHas, "assume-local-has", nil, "raw blob digests local already has (skips enumeration)")
	f.BoolVar(&pullFlags.keepGoing, "keep-going", false, "continue on per-image failure")
	f.IntVar(&pullFlags.retries, "retries", skopeoimageshare.DefaultRetries, "per-blob retry count")
	f.DurationVar(&pullFlags.retryMaxDelay, "retry-max-delay", skopeoimageshare.DefaultMaxDelay, "exp-backoff cap")
}

func runPull(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	if len(pullFlags.images) == 0 && len(args) == 0 {
		return fmt.Errorf("no images: use --image (repeatable) or positional args")
	}
	images := append([]string(nil), pullFlags.images...)
	images = append(images, args...)

	if err := validateRemoteTarget(remoteTarget); err != nil {
		return err
	}

	local, err := skopeoimageshare.NewLocal(ctx, skopeoimageshare.LocalConfig{
		BaseDir:   pullFlags.dataDir,
		Transport: pullFlags.localTransport,
		OCIPath:   pullFlags.localPath,
	})
	if err != nil {
		return err
	}

	if err := ssh.Probe(ctx, remoteTarget); err != nil {
		return fmt.Errorf("ssh probe: %w", err)
	}

	remote, err := skopeoimageshare.NewRemote(ctx, skopeoimageshare.RemoteConfig{
		Target:    remoteTarget,
		Transport: pullFlags.remoteTransport,
		OCIPath:   pullFlags.remotePath,
	})
	if err != nil {
		return err
	}
	defer remote.Close()

	if _, err := local.Skopeo().Version(ctx); err != nil {
		return fmt.Errorf("local skopeo: %w", err)
	}
	if _, err := remote.Skopeo().Version(ctx); err != nil {
		return fmt.Errorf("remote skopeo: %w", err)
	}

	pa := skopeoimageshare.PullArgs{
		Images:          images,
		LocalTransport:  pullFlags.localTransport,
		LocalPath:       pullFlags.localPath,
		RemoteTransport: pullFlags.remoteTransport,
		RemotePath:      pullFlags.remotePath,
		DataDir:         local.BaseDir,
		Jobs:            pullFlags.jobs,
		DryRun:          pullFlags.dryRun,
		AssumeLocalHas:  pullFlags.assumeLocalHas,
		KeepGoing:       pullFlags.keepGoing,
		Retries:         pullFlags.retries,
		RetryMaxDelay:   pullFlags.retryMaxDelay,
	}

	res, err := skopeoimageshare.Pull(ctx, pa, local.PullSide(), remote.PullPeerSide())
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
