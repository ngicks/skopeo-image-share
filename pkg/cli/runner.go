// Package cli holds the external-command runner abstraction shared
// by [./skopeo] and [./docker]. The [Runner] interface is the
// minimal contract those wrappers depend on; [LocalRunner] runs on
// this machine via [exec.CommandContext] and [SshRunner] runs on a
// remote host by spawning the system ssh binary (see [./ssh]).
package cli

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/ngicks/go-common/contextkey"
)

// Runner runs an external command argv (excluding argv[0]) and
// returns its captured stdout. Implementations are responsible for
// argv redaction in logs and for error wrapping (typically
// [*CommandError]).
type Runner interface {
	// Run executes argv and returns the captured stdout. argv[0] is
	// the "logical" subcommand list — the implementation owns choice
	// of executable name, working directory, env, etc.
	Run(ctx context.Context, argv []string) ([]byte, error)
}

// LocalRunner is a [Runner] backed by [exec.CommandContext]. The
// exe name is the binary on $PATH (e.g. "skopeo", "podman",
// "docker").
type LocalRunner struct {
	Exe string
	// StderrTailBytes caps how much trailing stderr is included in
	// the returned [*CommandError] on non-zero exit. Default 4096.
	StderrTailBytes int
}

// NewLocalRunner returns a [LocalRunner] for exe (looked up in $PATH
// at process invocation time).
func NewLocalRunner(exe string) *LocalRunner {
	return &LocalRunner{Exe: exe, StderrTailBytes: 4096}
}

// Run implements [Runner].
func (r *LocalRunner) Run(ctx context.Context, argv []string) ([]byte, error) {
	full := append([]string{r.Exe}, argv...)
	logger := contextkey.ValueSlogLoggerDefault(ctx)
	logger.LogAttrs(ctx, slog.LevelDebug, "exec",
		slog.String("exe", r.Exe),
		slog.Any("argv", RedactArgv(full)),
	)

	cmd := exec.CommandContext(ctx, r.Exe, argv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	if stderr.Len() > 0 {
		logger.LogAttrs(ctx, slog.LevelDebug, "exec stderr",
			slog.String("exe", r.Exe),
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
			Argv:       RedactArgv(full),
			ExitCode:   exit,
			StderrTail: TailBytes(stderr.Bytes(), tail),
			Err:        err,
		}
	}
	return stdout.Bytes(), nil
}

// CommandError wraps a non-zero exit from an external process. The
// Err field is the underlying error from [exec.Cmd.Run] (or
// equivalent).
type CommandError struct {
	Argv       []string
	ExitCode   int
	StderrTail string
	Err        error
}

// Error implements error.
func (e *CommandError) Error() string {
	return fmt.Sprintf(
		"command %q failed: exit %d: %s: %v",
		strings.Join(e.Argv, " "),
		e.ExitCode,
		strings.TrimSpace(e.StderrTail),
		e.Err,
	)
}

// Unwrap implements errors.Unwrap.
func (e *CommandError) Unwrap() error { return e.Err }

// SensitiveFlags lists argv flags whose value should be redacted in
// debug logs (and in [*CommandError]).
var SensitiveFlags = map[string]struct{}{
	"--creds":          {},
	"--src-creds":      {},
	"--dest-creds":     {},
	"--authfile":       {},
	"--password":       {},
	"--password-stdin": {},
}

// RedactArgv returns a copy of argv with values of [SensitiveFlags]
// replaced by "<redacted>". Both `--flag value` and `--flag=value`
// forms are handled.
func RedactArgv(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = a
		if eq := strings.IndexByte(a, '='); eq > 0 {
			if _, sensitive := SensitiveFlags[a[:eq]]; sensitive {
				out[i] = a[:eq] + "=<redacted>"
				continue
			}
		}
	}
	for i := 1; i < len(out); i++ {
		if _, sensitive := SensitiveFlags[out[i-1]]; sensitive {
			out[i] = "<redacted>"
		}
	}
	return out
}

// TailBytes returns at most max trailing bytes of b as a string.
// Used to cap the size of stderr captured into a [*CommandError].
func TailBytes(b []byte, max int) string {
	if max <= 0 || len(b) <= max {
		return string(b)
	}
	return string(b[len(b)-max:])
}
