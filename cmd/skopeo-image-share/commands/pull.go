package commands

import (
	"fmt"
	"time"

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
	remoteHost      string
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
	f.StringVar(&pullFlags.remoteHost, "remote-host", "", "user@host[:port]")
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

	if pullFlags.remoteHost == "" {
		return fmt.Errorf("--remote-host is required")
	}
	if len(pullFlags.images) == 0 && len(args) == 0 {
		return fmt.Errorf("no images: use --image (repeatable) or positional args")
	}
	images := append([]string(nil), pullFlags.images...)
	images = append(images, args...)

	target, err := skopeoimageshare.ParseSSHTarget(pullFlags.remoteHost)
	if err != nil {
		return err
	}

	localBase := pullFlags.dataDir
	if localBase == "" {
		localBase, err = skopeoimageshare.DefaultBaseDir()
		if err != nil {
			return err
		}
	}
	localStore := skopeoimageshare.NewStore(localBase)
	if err := localStore.EnsureLayout(ctx); err != nil {
		return err
	}

	if err := skopeoimageshare.ProbeSSH(ctx, target.Host); err != nil {
		return fmt.Errorf("ssh probe: %w", err)
	}

	remote, err := skopeoimageshare.NewRemote(ctx, target)
	if err != nil {
		return err
	}
	defer remote.Close()

	localSk := skopeoimageshare.NewSkopeo(skopeoimageshare.NewLocalRunner("skopeo"))
	remoteSk := remote.Skopeo()

	if _, err := localSk.Version(ctx); err != nil {
		return fmt.Errorf("local skopeo: %w", err)
	}
	if _, err := remoteSk.Version(ctx); err != nil {
		return fmt.Errorf("remote skopeo: %w", err)
	}

	remoteBase, err := remote.ResolveBaseDir(ctx)
	if err != nil {
		return fmt.Errorf("remote base dir: %w", err)
	}

	pa := skopeoimageshare.PullArgs{
		Images:          images,
		LocalTransport:  pullFlags.localTransport,
		LocalPath:       pullFlags.localPath,
		RemoteTransport: pullFlags.remoteTransport,
		RemotePath:      pullFlags.remotePath,
		DataDir:         localBase,
		Jobs:            pullFlags.jobs,
		DryRun:          pullFlags.dryRun,
		AssumeLocalHas:  pullFlags.assumeLocalHas,
		KeepGoing:       pullFlags.keepGoing,
		Retries:         pullFlags.retries,
		RetryMaxDelay:   pullFlags.retryMaxDelay,
	}

	localFS, err := skopeoimageshare.NewLocalFS(localBase)
	if err != nil {
		return err
	}
	local := skopeoimageshare.PullSide{
		Skopeo:    localSk,
		FS:        localFS,
		BaseDir:   localBase,
		Transport: pullFlags.localTransport,
		OCIPath:   pullFlags.localPath,
	}
	switch pullFlags.localTransport {
	case skopeoimageshare.TransportContainersStorage:
		local.Lister = skopeoimageshare.NewPodman(skopeoimageshare.NewLocalRunner("podman"))
	case skopeoimageshare.TransportDockerDaemon:
		local.Lister = skopeoimageshare.NewDocker(skopeoimageshare.NewLocalRunner("docker"))
	}

	peer := skopeoimageshare.PullPeerSide{
		Skopeo:    remoteSk,
		FS:        skopeoimageshare.NewSFTPFS(remote.SFTPClient(), remoteBase),
		BaseDir:   remoteBase,
		Transport: pullFlags.remoteTransport,
		OCIPath:   pullFlags.remotePath,
	}

	res, err := skopeoimageshare.Pull(ctx, pa, local, peer)
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
