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

// Remote bundles a live SFTP client (talking to an `ssh -s sftp`
// subprocess) with a force-close goroutine that fires after a
// 2-second grace period when ctx is cancelled. SSH transport is
// delegated entirely to the system ssh binary; auth, host-key
// verification, ProxyCommand etc. flow through the user's ssh config.
//
// Build via [NewRemote]; close with [Remote.Close].
type Remote struct {
	target ssh.Target
	runner *cli.SshRunner

	mu      sync.Mutex
	sftpCmd *exec.Cmd
	sftp    *sftp.Client
	closed  bool

	// cancelWatch is called by Close to stop the force-close goroutine.
	cancelWatch context.CancelFunc
}

// NewRemote spawns `ssh -s sftp` and wires its pipes into a sftp
// client via [sftp.NewClientPipe], then starts the force-close
// goroutine.
func NewRemote(ctx context.Context, target ssh.Target) (*Remote, error) {
	var stderrBuf bytes.Buffer
	cmd, stdout, stdin, err := ssh.Subsystem(ctx, target, &stderrBuf)
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
		target:  target,
		runner:  cli.NewSshRunner(target, ""),
		sftpCmd: cmd,
		sftp:    sftpC,
	}
	r.startWatch(ctx)
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

// SFTPRooted returns an [*sftpfs.SftpFs] rooted at base.
func (r *Remote) SFTPRooted(base string) *sftpfs.SftpFs { return sftpfs.New(r.sftp, base) }

// SFTPClient returns the underlying *sftp.Client (for advanced use).
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

// Skopeo returns a [skopeo.Skopeo] wrapper that runs `skopeo` on
// the remote.
func (r *Remote) Skopeo() *skopeo.Skopeo {
	return skopeo.New(cli.NewSshRunner(r.target, "skopeo"))
}

// Podman returns a [docker.Podman] wrapper that runs `podman` on the
// remote.
func (r *Remote) Podman() *docker.Podman {
	return docker.NewPodman(cli.NewSshRunner(r.target, "podman"))
}

// Docker returns a [docker.Docker] wrapper that runs `docker` on the
// remote.
func (r *Remote) Docker() *docker.Docker {
	return docker.NewDocker(cli.NewSshRunner(r.target, "docker"))
}
