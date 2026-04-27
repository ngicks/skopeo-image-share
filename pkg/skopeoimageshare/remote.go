package skopeoimageshare

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ngicks/skopeo-image-share/pkg/cli"
	"github.com/ngicks/skopeo-image-share/pkg/cli/docker"
	"github.com/ngicks/skopeo-image-share/pkg/cli/skopeo"
	"github.com/ngicks/skopeo-image-share/pkg/cli/ssh"
	"github.com/ngicks/skopeo-image-share/pkg/sftpfs"
	"github.com/pkg/sftp"
)

// Remote bundles the remote-side dependencies the push/pull
// orchestrator consumes: a live SFTP client (talking to an
// `ssh -s sftp` subprocess), a force-close goroutine that fires after
// [ForceCloseGrace] when ctx is cancelled, the resolved remote base
// dir, and the remote skopeo / podman / docker wrappers. SSH transport
// is delegated entirely to the system ssh binary; auth, host-key
// verification, ProxyCommand etc. flow through the user's ssh config.
//
// Build via [NewRemote]; emit [PullPeerSide] / [PushPeerSide] snapshots
// via the methods of the same name; close with [Remote.Close].
type Remote struct {
	BaseDir   string
	Transport string
	OCIPath   string

	target ssh.Target
	runner *cli.SshRunner

	mu      sync.Mutex
	sftpCmd *exec.Cmd
	sftp    *sftp.Client
	closed  bool

	cancelWatch context.CancelFunc

	skopeoCli *skopeo.Skopeo
	lister    listInterface
	fs        FS
}

// RemoteConfig configures [NewRemote].
//
//   - Target is the SSH destination (required).
//   - Transport is required: one of [TransportContainersStorage],
//     [TransportDockerDaemon], or [TransportOCI].
//   - OCIPath is required when Transport == [TransportOCI].
type RemoteConfig struct {
	Target    ssh.Target
	Transport string
	OCIPath   string
}

// NewRemote spawns `ssh -s sftp`, wires its pipes into a sftp client
// via [sftp.NewClientPipe], starts the force-close goroutine, then
// resolves BaseDir on the remote and builds the remote skopeo wrapper
// + a transport-appropriate lister + an FS rooted at BaseDir.
func NewRemote(ctx context.Context, cfg RemoteConfig) (*Remote, error) {
	if cfg.Transport == "" {
		return nil, errors.New("remote: transport unset")
	}
	var stderrBuf bytes.Buffer
	cmd, stdout, stdin, err := ssh.Subsystem(ctx, cfg.Target, &stderrBuf)
	if err != nil {
		return nil, err
	}
	sftpC, err := sftp.NewClientPipe(stdout, stdin)
	if err != nil {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr != "" {
			return nil, fmt.Errorf("sftp: %w: %s", err, stderr)
		}
		return nil, fmt.Errorf("sftp: %w", err)
	}
	r := &Remote{
		Transport: cfg.Transport,
		OCIPath:   cfg.OCIPath,
		target:    cfg.Target,
		runner:    cli.NewSshRunner(cfg.Target, ""),
		sftpCmd:   cmd,
		sftp:      sftpC,
	}
	r.startWatch(ctx)

	base, err := r.ResolveBaseDir(ctx)
	if err != nil {
		_ = r.Close()
		return nil, fmt.Errorf("remote: resolve base dir: %w", err)
	}
	r.BaseDir = base
	r.fs = sftpfs.New(sftpC, base)
	r.skopeoCli = skopeo.New(cli.NewSshRunner(cfg.Target, "skopeo"))
	switch cfg.Transport {
	case TransportContainersStorage:
		r.lister = docker.NewPodman(cli.NewSshRunner(cfg.Target, "podman"))
	case TransportDockerDaemon:
		r.lister = docker.NewDocker(cli.NewSshRunner(cfg.Target, "docker"))
	}
	return r, nil
}

// startWatch installs a goroutine that, when ctx is cancelled, waits
// ForceCloseGrace and then closes the underlying SFTP client and
// kills the ssh subprocess — unblocking any pending Read/Write that
// didn't honor the cooperative per-read cancellation.
func (r *Remote) startWatch(parent context.Context) {
	wctx, cancel := context.WithCancel(parent)
	r.cancelWatch = cancel
	go func() {
		<-wctx.Done()
		select {
		case <-time.After(ForceCloseGrace):
		default:
		}
		r.forceClose()
	}()
}

// ForceCloseGrace is the grace period before [Remote] hard-closes its
// SFTP client / ssh subprocess on context cancellation.
var ForceCloseGrace = 2 * time.Second

// Close closes the SFTP client and waits for the ssh subprocess to
// exit. Safe to call multiple times.
func (r *Remote) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if r.cancelWatch != nil {
		r.cancelWatch()
	}
	var firstErr error
	if r.sftp != nil {
		if err := r.sftp.Close(); err != nil {
			firstErr = err
		}
	}
	if r.sftpCmd != nil && r.sftpCmd.Process != nil {
		if err := r.waitCmd(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (r *Remote) forceClose() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	if r.sftp != nil {
		_ = r.sftp.Close()
	}
	if r.sftpCmd != nil && r.sftpCmd.Process != nil {
		_ = r.sftpCmd.Process.Kill()
		_ = r.waitCmd()
	}
}

// waitCmd waits for the ssh subprocess to exit, capping the wait at
// ForceCloseGrace before SIGKILL'ing it. Discards the well-known
// "signal: killed" error that we induce ourselves.
func (r *Remote) waitCmd() error {
	done := make(chan error, 1)
	go func() { done <- r.sftpCmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			return nil
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil
		}
		return err
	case <-time.After(ForceCloseGrace):
		_ = r.sftpCmd.Process.Kill()
		<-done
		return nil
	}
}

// SFTPRooted returns an [*sftpfs.SftpFs] rooted at base. For the
// resolved remote BaseDir, prefer [Remote.FS].
func (r *Remote) SFTPRooted(base string) *sftpfs.SftpFs { return sftpfs.New(r.sftp, base) }

// SFTPClient returns the underlying *sftp.Client.
func (r *Remote) SFTPClient() *sftp.Client { return r.sftp }

// Run runs argv on the remote by spawning a fresh `ssh ... -- <argv>`
// subprocess and returns the captured stdout. Delegates to the
// embedded [*cli.SshRunner]; gates on the remote being open first.
func (r *Remote) Run(ctx context.Context, argv []string) ([]byte, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("remote: closed")
	}
	r.mu.Unlock()
	return r.runner.Run(ctx, argv)
}

// ResolveBaseDir returns the remote's
// `${XDG_DATA_HOME:-$HOME/.local/share}/skopeo-image-share` path.
// printf is used (no trailing newline) for clean parsing.
func (r *Remote) ResolveBaseDir(ctx context.Context) (string, error) {
	out, err := r.Run(ctx, []string{
		"sh", "-c",
		`printf %s "${XDG_DATA_HOME:-$HOME/.local/share}/` + AppDirName + `"`,
	})
	if err != nil {
		return "", err
	}
	base := strings.TrimSpace(string(out))
	if base == "" {
		return "", errors.New("remote: empty base dir")
	}
	return base, nil
}

// Skopeo returns the remote skopeo wrapper.
func (r *Remote) Skopeo() *skopeo.Skopeo { return r.skopeoCli }

// FS returns the remote [FS] rooted at BaseDir.
func (r *Remote) FS() FS { return r.fs }

// PullPeerSide returns the snapshot consumed by [Pull] as the
// source-of-truth side.
func (r *Remote) PullPeerSide() PullPeerSide {
	return PullPeerSide{
		Skopeo:    r.skopeoCli,
		FS:        r.fs,
		BaseDir:   r.BaseDir,
		Transport: r.Transport,
		OCIPath:   r.OCIPath,
	}
}

// PushPeerSide returns the snapshot consumed by [Push] as the peer
// (destination) side.
func (r *Remote) PushPeerSide() PushPeerSide {
	return PushPeerSide{
		Skopeo:    r.skopeoCli,
		FS:        r.fs,
		BaseDir:   r.BaseDir,
		Transport: r.Transport,
		OCIPath:   r.OCIPath,
		Lister:    r.lister,
	}
}
