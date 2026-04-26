package commands

import (
	"fmt"
	"time"

	"github.com/ngicks/skopeo-image-share/pkg/cli"
	"github.com/ngicks/skopeo-image-share/pkg/cli/skopeo"
	"github.com/ngicks/skopeo-image-share/pkg/cli/ssh"
	"github.com/ngicks/skopeo-image-share/pkg/sftpfs"
	"github.com/ngicks/skopeo-image-share/pkg/skopeoimageshare"
	"github.com/spf13/cobra"
)

var pushCmd = &cobra.Command{
	Use:   "push",
	Short: "Push images from local to remote.",
	Args:  cobra.ArbitraryArgs,
	RunE:  runPush,
}

var pushFlags struct {
	images          []string
	localTransport  string
	localPath       string
	remoteTransport string
	remotePath      string
	dataDir         string
	jobs            int
	dryRun          bool
	assumeRemoteHas []string
	keepGoing       bool
	retries         int
	retryMaxDelay   time.Duration
}

func init() {
	rootCmd.AddCommand(pushCmd)

	f := pushCmd.Flags()
	f.StringSliceVar(&pushFlags.images, "image", nil, "image ref to push (repeatable)")
	f.StringVar(&pushFlags.localTransport, "local-transport", "containers-storage", "containers-storage|docker-daemon|oci")
	f.StringVar(&pushFlags.localPath, "local-path", "", "local oci: dir (only when --local-transport=oci)")
	bindRemoteTargetFlags(f)
	f.StringVar(&pushFlags.remoteTransport, "remote-transport", "containers-storage", "containers-storage|docker-daemon|oci")
	f.StringVar(&pushFlags.remotePath, "remote-path", "", "remote oci: dir (only when --remote-transport=oci)")
	f.StringVar(&pushFlags.dataDir, "data-dir", "", "override $XDG_DATA_HOME data dir")
	f.IntVar(&pushFlags.jobs, "jobs", 4, "per-blob parallelism")
	f.BoolVar(&pushFlags.dryRun, "dry-run", false, "no mutation; emit a plan instead")
	f.StringSliceVar(&pushFlags.assumeRemoteHas, "assume-remote-has", nil, "raw blob digests the peer already has (skips enumeration)")
	f.BoolVar(&pushFlags.keepGoing, "keep-going", false, "continue on per-image failure")
	f.IntVar(&pushFlags.retries, "retries", skopeoimageshare.DefaultRetries, "per-blob retry count")
	f.DurationVar(&pushFlags.retryMaxDelay, "retry-max-delay", skopeoimageshare.DefaultMaxDelay, "exp-backoff cap")
}

func runPush(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	if len(pushFlags.images) == 0 && len(args) == 0 {
		return fmt.Errorf("no images: use --image (repeatable) or positional args")
	}
	images := append([]string(nil), pushFlags.images...)
	images = append(images, args...)

	if err := validateRemoteTarget(remoteTarget); err != nil {
		return err
	}

	localBase := pushFlags.dataDir
	var err error
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

	if err := ssh.Probe(ctx, remoteTarget); err != nil {
		return fmt.Errorf("ssh probe: %w", err)
	}

	remote, err := skopeoimageshare.NewRemote(ctx, remoteTarget)
	if err != nil {
		return err
	}
	defer remote.Close()

	localSk := skopeo.New(cli.NewLocalRunner("skopeo"))
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

	pa := skopeoimageshare.PushArgs{
		Images:          images,
		LocalTransport:  pushFlags.localTransport,
		LocalPath:       pushFlags.localPath,
		RemoteTransport: pushFlags.remoteTransport,
		RemotePath:      pushFlags.remotePath,
		DataDir:         localBase,
		Jobs:            pushFlags.jobs,
		DryRun:          pushFlags.dryRun,
		AssumeRemoteHas: pushFlags.assumeRemoteHas,
		KeepGoing:       pushFlags.keepGoing,
		Retries:         pushFlags.retries,
		RetryMaxDelay:   pushFlags.retryMaxDelay,
	}

	localFS, err := skopeoimageshare.NewLocalFS(localBase)
	if err != nil {
		return err
	}
	local := skopeoimageshare.PushSide{
		Skopeo:    localSk,
		FS:        localFS,
		BaseDir:   localBase,
		Transport: pushFlags.localTransport,
		OCIPath:   pushFlags.localPath,
	}
	peer := skopeoimageshare.PushPeerSide{
		Skopeo:    remoteSk,
		FS:        sftpfs.New(remote.SFTPClient(), remoteBase),
		BaseDir:   remoteBase,
		Transport: pushFlags.remoteTransport,
		OCIPath:   pushFlags.remotePath,
	}
	switch pushFlags.remoteTransport {
	case skopeoimageshare.TransportContainersStorage:
		peer.Lister = remote.Podman()
	case skopeoimageshare.TransportDockerDaemon:
		peer.Lister = remote.Docker()
	}

	res, err := skopeoimageshare.Push(ctx, pa, local, peer)
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
