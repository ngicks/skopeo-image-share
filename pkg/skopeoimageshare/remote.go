package skopeoimageshare

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ngicks/skopeo-image-share/pkg/cli"
	"github.com/ngicks/skopeo-image-share/pkg/cli/docker"
	"github.com/ngicks/skopeo-image-share/pkg/cli/skopeo"
	"github.com/ngicks/skopeo-image-share/pkg/cli/ssh"
	"github.com/ngicks/skopeo-image-share/pkg/imageref"
	"github.com/ngicks/skopeo-image-share/pkg/sftpfs"
	"github.com/pkg/sftp"
)

// Remote is the abstract peer that [Local.Push] and [Local.Pull]
// drive. The SSH+SFTP-backed implementation returned by [NewRemote]
// satisfies it; custom transports (a different network transport, a
// read-only registry mirror, an in-memory test double) plug in by
// implementing this interface.
//
// Read-only implementations return true from [Remote.ReadOnly]. The
// orchestrator refuses to push to a read-only peer; pull operations
// that would mutate the peer (the `skopeo copy` dump step) fail
// naturally via the underlying transport.
type Remote interface {
	// Close releases any subsystem resources (e.g. the ssh+sftp
	// subprocess for [NewRemote]). Safe to call multiple times.
	Close() error

	// BaseDir is the absolute path of the peer's data dir
	// (`<base>` in the on-disk layout described under [Store]).
	BaseDir() string
	// Transport is one of [skopeo.TransportContainersStorage],
	// [skopeo.TransportDockerDaemon], or [skopeo.TransportOci].
	Transport() skopeo.Transport
	// OCIPath is the path passed via `oci:<dir>`; only meaningful when
	// Transport == [skopeo.TransportOci].
	OCIPath() string
	// Skopeo is the skopeo wrapper bound to this peer.
	Skopeo() SkopeoLike
	// FS is rooted at BaseDir; orchestrator-facing paths are FS-relative.
	FS() Fs
	// Lister is the docker / podman wrapper for live image
	// enumeration. Returns nil for [skopeo.TransportOci].
	Lister() Lister
	// ReadOnly reports whether mutating operations targeting this peer
	// should be rejected.
	ReadOnly() bool

	// Validate runs peer-side sanity checks (e.g. confirms the remote
	// skopeo is present and runnable). Implementations are expected to
	// be cheap to call repeatedly — typically by caching the first
	// successful result. [Local.Push] / [Local.Pull] call this before
	// any work happens.
	Validate(ctx context.Context) error

	// Dump runs `skopeo copy <Transport>:<ref> oci:<store-tag-dir>`,
	// staging ref into the peer's store layout. Returns the absolute
	// peer-side tag directory.
	Dump(ctx context.Context, ref imageref.ImageRef) (string, error)
	// List returns the digest set of every blob the peer has,
	// including the share/ inventory.
	List(ctx context.Context) (DigestSet, error)
}

// RemoteConfig configures [NewRemote].
//
//   - Target is the SSH destination (required).
//   - Transport is required: one of [skopeo.TransportContainersStorage],
//     [skopeo.TransportDockerDaemon], or [skopeo.TransportOci].
//   - OCIPath is required when Transport == [skopeo.TransportOci].
type RemoteConfig struct {
	Target    ssh.Target
	Transport skopeo.Transport
	OCIPath   string
}

// Compile-time check: [*sshRemote] satisfies [Remote].
var _ Remote = (*sshRemote)(nil)

// sshRemote is the SSH+SFTP-backed [Remote]. SSH transport is delegated
// entirely to the system ssh binary; auth, host-key verification,
// ProxyCommand etc. flow through the user's ssh config.
type sshRemote struct {
	baseDir   string
	transport skopeo.Transport
	ociPath   string

	target ssh.Target
	runner *cli.SshRunner

	mu      sync.Mutex
	sftpCmd *exec.Cmd
	sftp    *sftp.Client
	closed  bool

	cancelWatch context.CancelFunc

	skopeoCli SkopeoLike
	lister    Lister
	fs        Fs

	validateOnce sync.Once
	validateErr  error
}

// NewRemote spawns `ssh -s sftp`, wires its pipes into a sftp client
// via [sftp.NewClientPipe], starts the force-close goroutine, then
// resolves BaseDir on the remote and builds the remote skopeo wrapper +
// a transport-appropriate lister + an FS rooted at BaseDir.
func NewRemote(ctx context.Context, cfg RemoteConfig) (Remote, error) {
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
	r := &sshRemote{
		transport: cfg.Transport,
		ociPath:   cfg.OCIPath,
		target:    cfg.Target,
		runner:    cli.NewSshRunner(cfg.Target, ""),
		sftpCmd:   cmd,
		sftp:      sftpC,
	}
	r.startWatch(ctx)

	base, err := r.resolveBaseDir(ctx)
	if err != nil {
		_ = r.Close()
		return nil, fmt.Errorf("remote: resolve base dir: %w", err)
	}
	r.baseDir = base
	r.fs = sftpfs.New(sftpC, base)
	r.skopeoCli = &skopeo.Skopeo{Runner: cli.NewSshRunner(cfg.Target, "skopeo")}
	switch cfg.Transport {
	case skopeo.TransportContainersStorage:
		r.lister = docker.NewPodman(cli.NewSshRunner(cfg.Target, "podman"))
	case skopeo.TransportDockerDaemon:
		r.lister = docker.NewDocker(cli.NewSshRunner(cfg.Target, "docker"))
	}
	return r, nil
}

// startWatch installs a goroutine that, when ctx is cancelled, waits
// ForceCloseGrace and then closes the underlying SFTP client and
// kills the ssh subprocess — unblocking any pending Read/Write that
// didn't honor the cooperative per-read cancellation.
func (r *sshRemote) startWatch(parent context.Context) {
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

// ForceCloseGrace is the grace period before the SSH-backed [Remote]
// hard-closes its SFTP client / ssh subprocess on context cancellation.
var ForceCloseGrace = 2 * time.Second

// Close implements [Remote].
func (r *sshRemote) Close() error {
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

func (r *sshRemote) forceClose() {
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
func (r *sshRemote) waitCmd() error {
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

// runRemote runs argv on the remote by spawning a fresh
// `ssh ... -- <argv>` subprocess and returns the captured stdout.
func (r *sshRemote) runRemote(ctx context.Context, argv []string) ([]byte, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("remote: closed")
	}
	r.mu.Unlock()
	return r.runner.Run(ctx, argv)
}

// resolveBaseDir returns the remote's
// `${XDG_DATA_HOME:-$HOME/.local/share}/skopeo-image-share` path.
// printf is used (no trailing newline) for clean parsing.
func (r *sshRemote) resolveBaseDir(ctx context.Context) (string, error) {
	out, err := r.runRemote(ctx, []string{
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

// BaseDir implements [Remote].
func (r *sshRemote) BaseDir() string { return r.baseDir }

// Transport implements [Remote].
func (r *sshRemote) Transport() skopeo.Transport { return r.transport }

// OCIPath implements [Remote].
func (r *sshRemote) OCIPath() string { return r.ociPath }

// Skopeo implements [Remote].
func (r *sshRemote) Skopeo() SkopeoLike { return r.skopeoCli }

// FS implements [Remote].
func (r *sshRemote) FS() Fs { return r.fs }

// Lister implements [Remote].
func (r *sshRemote) Lister() Lister { return r.lister }

// ReadOnly implements [Remote]. The SSH-backed remote always reports
// false; build a custom [Remote] to surface a read-only peer.
func (r *sshRemote) ReadOnly() bool { return false }

// Validate implements [Remote]: confirms the remote skopeo binary
// runs. Cached after the first invocation.
func (r *sshRemote) Validate(ctx context.Context) error {
	r.validateOnce.Do(func() {
		if _, err := r.skopeoCli.Version(ctx); err != nil {
			r.validateErr = fmt.Errorf("remote skopeo: %w", err)
		}
	})
	return r.validateErr
}

// Dump implements [Remote].
func (r *sshRemote) Dump(ctx context.Context, ref imageref.ImageRef) (string, error) {
	if err := r.Validate(ctx); err != nil {
		return "", err
	}
	return dumpRemote(ctx, r.transport, r.baseDir, r.skopeoCli, r.fs, ref)
}

// List implements [Remote].
func (r *sshRemote) List(ctx context.Context) (DigestSet, error) {
	return listAt(ctx, r.transport, r.skopeoCli, r.fs, r.baseDir, r.lister)
}

// dumpRemote dumps ref into the peer's oci-layout. Mirrors
// [Local.Dump] but slash-normalizes the absolute paths handed to the
// remote skopeo CLI (peer's filesystem is POSIX even when the host
// running this binary is not).
func dumpRemote(ctx context.Context, transport skopeo.Transport, baseDir string, sk SkopeoLike, fs Fs, ref imageref.ImageRef) (string, error) {
	store := NewStore(baseDir)
	tagDirNative, err := store.DumpDir(ref)
	if err != nil {
		return "", err
	}
	tagDirAbs := filepath.ToSlash(tagDirNative)
	tagDirRel, err := RelDumpDir(ref)
	if err != nil {
		return "", err
	}
	shareAbs := filepath.ToSlash(store.ShareDir())
	if err := fs.MkdirAll(tagDirRel, 0o755); err != nil {
		return "", fmt.Errorf("dump: mkdir %s: %w", tagDirRel, err)
	}
	if err := sk.Copy(ctx,
		skopeo.TransportRef{Transport: transport, Arg1: ref.String()},
		skopeo.TransportRef{Transport: skopeo.TransportOci, Arg1: tagDirAbs, Arg2: ref.String()},
		shareAbs,
	); err != nil {
		return "", fmt.Errorf("dump: skopeo copy: %w", err)
	}
	return tagDirAbs, nil
}

// listAt dispatches to [Enumerate] using the right lister for transport.
func listAt(ctx context.Context, transport skopeo.Transport, sk SkopeoLike, fs Fs, baseDir string, lister Lister) (DigestSet, error) {
	cfg := EnumerateConfig{
		Transport: transport,
		Skopeo:    sk,
		Fs:        fs,
		BaseDir:   baseDir,
	}
	switch transport {
	case skopeo.TransportContainersStorage:
		cfg.Podman = lister
	case skopeo.TransportDockerDaemon:
		cfg.Docker = lister
	}
	return Enumerate(ctx, cfg)
}
