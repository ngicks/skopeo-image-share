package cli

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/ngicks/go-common/contextkey"
	"github.com/ngicks/skopeo-image-share/pkg/cli/ssh"
)

// SshRunner is a [Runner] that spawns a fresh `ssh ... -- <argv>`
// subprocess per call. SSH transport, auth, host-key verification and
// ProxyCommand/Include flow through the system ssh binary's normal
// config codepath — this runner never touches keys, agents or
// known_hosts directly.
//
// The remote shell is `sh -c <argv>`: argv tokens are single-quoted
// before transmission so meta-characters are inert.
type SshRunner struct {
	Target ssh.Target
	// StderrTailBytes caps how much trailing stderr is included in
	// the returned [*CommandError] on non-zero exit. Default 4096.
	StderrTailBytes int
}

// NewSshRunner returns an [SshRunner] for target.
func NewSshRunner(target ssh.Target) *SshRunner {
	return &SshRunner{Target: target, StderrTailBytes: 4096}
}

// Run implements [Runner]. argv is sent verbatim to the remote shell;
// argv[0] is the remote executable. argv must be non-empty.
func (r *SshRunner) Run(ctx context.Context, argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("cli: empty argv")
	}

	logger := contextkey.ValueSlogLoggerDefault(ctx)
	logger.LogAttrs(ctx, slog.LevelDebug, "ssh.exec",
		slog.Any("argv", RedactArgv(argv)),
		slog.String("host", r.Target.String()),
	)

	sshArgs := append(ssh.BinaryArgs(r.Target), "--", shellQuote(argv))
	cmd := exec.CommandContext(ctx, "ssh", sshArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	if stderr.Len() > 0 {
		logger.LogAttrs(ctx, slog.LevelDebug, "ssh.exec.stderr",
			slog.String("stderr", stderr.String()),
		)
	}

	if err != nil {
		exit := -1
		if cmd.ProcessState != nil {
			exit = cmd.ProcessState.ExitCode()
		}
		tail := r.StderrTailBytes
		if tail <= 0 {
			tail = 4096
		}
		return stdout.Bytes(), &CommandError{
			Argv:       RedactArgv(argv),
			ExitCode:   exit,
			StderrTail: TailBytes(stderr.Bytes(), tail),
			Err:        err,
		}
	}
	return stdout.Bytes(), nil
}

// shellQuote builds a single sh-safe word from argv. Each token is
// single-quoted so meta-characters (`'$|;&`) are inert; the whole
// string is fed to ssh as one argument so the remote sshd hands it to
// `sh -c`.
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
