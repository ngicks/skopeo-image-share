package skopeoimageshare

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ngicks/go-common/contextkey"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// SSHTarget is a parsed user@host[:port] target.
type SSHTarget struct {
	User string
	Host string
	Port int
}

// String reassembles the target.
func (t SSHTarget) String() string {
	if t.Port != 0 && t.Port != 22 {
		return fmt.Sprintf("%s@%s:%d", t.User, t.Host, t.Port)
	}
	return t.User + "@" + t.Host
}

// addr returns "host:port" for net.Dial.
func (t SSHTarget) addr() string {
	port := t.Port
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(t.Host, strconv.Itoa(port))
}

// ParseSSHTarget parses "user@host" or "user@host:port".
func ParseSSHTarget(s string) (SSHTarget, error) {
	at := strings.LastIndex(s, "@")
	if at <= 0 {
		return SSHTarget{}, fmt.Errorf("ssh target %q: expected user@host[:port]", s)
	}
	user := s[:at]
	rest := s[at+1:]
	host, portStr, err := net.SplitHostPort(rest)
	if err != nil {
		// no port — net.SplitHostPort fails. Treat the whole thing as
		// a host.
		return SSHTarget{User: user, Host: rest}, nil
	}
	if host == "" {
		return SSHTarget{}, fmt.Errorf("ssh target %q: empty host", s)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return SSHTarget{}, fmt.Errorf("ssh target %q: bad port: %w", s, err)
	}
	return SSHTarget{User: user, Host: host, Port: port}, nil
}

// sshClient is implemented by *ssh.Client; we abstract it so test code
// can inject a fake.
type sshClient interface {
	Underlying() *ssh.Client
}

type realSSHClient struct{ c *ssh.Client }

func (r *realSSHClient) Underlying() *ssh.Client { return r.c }

// DialSSH connects to target using default keys (~/.ssh/id_*) and
// SSH_AUTH_SOCK. Host keys are verified against ~/.ssh/known_hosts via
// [knownhosts.New].
func DialSSH(ctx context.Context, target SSHTarget) (*ssh.Client, error) {
	logger := contextkey.ValueSlogLoggerDefault(ctx)

	authMethods, err := defaultAuthMethods(logger)
	if err != nil {
		return nil, fmt.Errorf("ssh: assemble auth methods: %w", err)
	}

	hostKeyCB, err := defaultHostKeyCallback()
	if err != nil {
		return nil, fmt.Errorf("ssh: known_hosts: %w", err)
	}

	cfg := &ssh.ClientConfig{
		User:            target.User,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCB,
		Timeout:         10 * time.Second,
	}

	logger.LogAttrs(ctx, slog.LevelDebug, "ssh.dial",
		slog.String("addr", target.addr()),
		slog.String("user", target.User),
	)

	d := net.Dialer{Timeout: cfg.Timeout}
	conn, err := d.DialContext(ctx, "tcp", target.addr())
	if err != nil {
		return nil, fmt.Errorf("ssh: dial %s: %w", target.addr(), err)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, target.addr(), cfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh: handshake: %w", err)
	}
	return ssh.NewClient(c, chans, reqs), nil
}

// defaultAuthMethods returns ssh-agent (if SSH_AUTH_SOCK is set) and
// any of ~/.ssh/id_{rsa,ecdsa,ed25519} that exist and parse.
func defaultAuthMethods(logger *slog.Logger) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		conn, err := net.Dial("unix", sock)
		if err == nil {
			ag := agent.NewClient(conn)
			methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
			logger.Debug("ssh.auth.agent", "sock", sock)
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return methods, nil
	}
	for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
		p := filepath.Join(home, ".ssh", name)
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			continue
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if len(methods) == 0 {
		return nil, errors.New("no usable auth method (no agent, no readable id_*)")
	}
	return methods, nil
}

// defaultHostKeyCallback uses ~/.ssh/known_hosts.
func defaultHostKeyCallback() (ssh.HostKeyCallback, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	known := filepath.Join(home, ".ssh", "known_hosts")
	if _, err := os.Stat(known); err != nil {
		return nil, fmt.Errorf("missing %s; populate it before connecting", known)
	}
	return knownhosts.New(known)
}

// ProbeSSH executes `ssh -G <host>` and `ssh <host> true` and reports
// errors. This surfaces ProxyCommand/Include/etc. issues via the user's
// normal ssh config codepath before we attempt the in-process dial.
//
// This is a separate function (not a Remote method) because it runs
// before any in-process connection attempt.
func ProbeSSH(ctx context.Context, hostArg string) error {
	logger := contextkey.ValueSlogLoggerDefault(ctx)
	logger.LogAttrs(ctx, slog.LevelDebug, "ssh.probe.start",
		slog.String("host", hostArg),
	)

	if _, err := exec.LookPath("ssh"); err != nil {
		return fmt.Errorf("ssh probe: ssh binary not on PATH: %w", err)
	}

	{
		cmd := exec.CommandContext(ctx, "ssh", "-G", hostArg)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("ssh probe: ssh -G %s failed: %w: %s",
				hostArg, err, strings.TrimSpace(stderr.String()))
		}
	}
	{
		cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", hostArg, "true")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("ssh probe: ssh %s true failed: %w: %s",
				hostArg, err, strings.TrimSpace(stderr.String()))
		}
	}
	return nil
}

// Remote bundles a live ssh.Client + sftp.Client with a force-close
// goroutine that fires after a 2-second grace period when ctx is
// cancelled. Build via [NewRemote]; close with [Remote.Close].
type Remote struct {
	target SSHTarget

	mu     sync.Mutex
	ssh    *ssh.Client
	sftp   *sftp.Client
	closed bool

	// cancelWatch is called by Close to stop the force-close goroutine.
	cancelWatch context.CancelFunc
}

// NewRemote dials SSH+SFTP and starts the force-close goroutine.
func NewRemote(ctx context.Context, target SSHTarget) (*Remote, error) {
	sshC, err := DialSSH(ctx, target)
	if err != nil {
		return nil, err
	}
	sftpC, err := sftp.NewClient(sshC)
	if err != nil {
		_ = sshC.Close()
		return nil, fmt.Errorf("sftp: %w", err)
	}
	r := &Remote{target: target, ssh: sshC, sftp: sftpC}
	r.startWatch(ctx)
	return r, nil
}

// startWatch installs a goroutine that, when ctx is cancelled, waits
// ForceCloseGrace and then closes the underlying SSH/SFTP clients —
// unblocking any pending Read/Write that didn't honor the cooperative
// per-read cancellation.
func (r *Remote) startWatch(parent context.Context) {
	wctx, cancel := context.WithCancel(parent)
	r.cancelWatch = cancel
	go func() {
		<-wctx.Done()
		// either parent was cancelled, or Close fired cancelWatch
		select {
		case <-time.After(ForceCloseGrace):
		default:
		}
		r.forceClose()
	}()
}

// ForceCloseGrace is the grace period before [Remote] hard-closes its
// underlying SSH/SFTP clients on context cancellation.
var ForceCloseGrace = 2 * time.Second

// Close closes the SSH+SFTP clients and stops the force-close watcher.
// Safe to call multiple times.
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
	if r.ssh != nil {
		if err := r.ssh.Close(); err != nil && firstErr == nil {
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
	if r.ssh != nil {
		_ = r.ssh.Close()
	}
}

// SFTPRooted returns an [*SFTPFS] rooted at base. base is typically
// the peer's resolved data dir.
func (r *Remote) SFTPRooted(base string) *SFTPFS { return NewSFTPFS(r.sftp, base) }

// SFTPClient returns the underlying *sftp.Client (for advanced use).
func (r *Remote) SFTPClient() *sftp.Client { return r.sftp }

// SSHClient returns the underlying *ssh.Client.
func (r *Remote) SSHClient() *ssh.Client { return r.ssh }

// Run runs argv on the remote via a fresh SSH session and returns the
// captured stdout. stderr is included in the returned [*CommandError]
// on non-zero exit and otherwise streamed to slog at debug level.
func (r *Remote) Run(ctx context.Context, argv []string) ([]byte, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("remote: closed")
	}
	r.mu.Unlock()

	logger := contextkey.ValueSlogLoggerDefault(ctx)

	logger.LogAttrs(ctx, slog.LevelDebug, "ssh.exec",
		slog.Any("argv", redactArgv(argv)),
		slog.String("host", r.target.String()),
	)

	sess, err := r.ssh.NewSession()
	if err != nil {
		return nil, fmt.Errorf("ssh: new session: %w", err)
	}
	defer sess.Close()

	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	cmdLine := shellQuote(argv)
	if err := sess.Start(cmdLine); err != nil {
		return nil, fmt.Errorf("ssh: start %q: %w", cmdLine, err)
	}

	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()

	select {
	case err := <-done:
		if stderr.Len() > 0 {
			logger.LogAttrs(ctx, slog.LevelDebug, "ssh.exec.stderr",
				slog.String("stderr", stderr.String()),
			)
		}
		if err != nil {
			exit := -1
			var ee *ssh.ExitError
			if errors.As(err, &ee) {
				exit = ee.ExitStatus()
			}
			return stdout.Bytes(), &CommandError{
				Argv:       redactArgv(argv),
				ExitCode:   exit,
				StderrTail: tailBytes(stderr.Bytes(), 4096),
				Err:        err,
			}
		}
		return stdout.Bytes(), nil
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGTERM)
		return stdout.Bytes(), ctx.Err()
	}
}

// shellQuote builds a sh-safe command line for argv. Each token is
// single-quoted so meta-characters (`'$|;&`) are inert.
func shellQuote(argv []string) string {
	var b strings.Builder
	for i, a := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('\'')
		b.WriteString(strings.ReplaceAll(a, "'", `'\''`))
		b.WriteByte('\'')
	}
	return b.String()
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

// remoteRunner adapts [*Remote.Run] to the [CommandRunner] interface
// so [Skopeo]/[Podman]/[Docker] can drive a remote binary.
type remoteRunner struct {
	r   *Remote
	exe string
}

// Run implements [CommandRunner] by prepending r.exe to argv and
// dispatching via SSH.
func (rr *remoteRunner) Run(ctx context.Context, argv []string) ([]byte, error) {
	full := append([]string{rr.exe}, argv...)
	return rr.r.Run(ctx, full)
}

// Skopeo returns a [Skopeo] wrapper that runs `skopeo` on the remote.
func (r *Remote) Skopeo() *Skopeo { return NewSkopeo(&remoteRunner{r: r, exe: "skopeo"}) }

// Podman returns a [Podman] wrapper that runs `podman` on the remote.
func (r *Remote) Podman() *Podman { return NewPodman(&remoteRunner{r: r, exe: "podman"}) }

// Docker returns a [Docker] wrapper that runs `docker` on the remote.
func (r *Remote) Docker() *Docker { return NewDocker(&remoteRunner{r: r, exe: "docker"}) }
