// Package skopeo is a typed wrapper over the skopeo CLI. It does not
// look at flag spellings or the installed skopeo version; runtime
// errors surface via the [Runner] implementation.
package skopeo

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/ngicks/skopeo-image-share/pkg/cli"
)

type Transport string

const (
	TransportDir               Transport = "dir"
	TransportContainersStorage Transport = "containers-storage"
	TransportDocker            Transport = "docker"
	TransportDockerArchive     Transport = "docker-archive"
	TransportDockerDaemon      Transport = "docker-daemon"
	TransportOci               Transport = "oci"
)

type TransportRef struct {
	Transport Transport
	// ref for "containers-storage", "docker" and "docker-daemon", path for "dir", "docker-archive" and "oci"
	Arg1 string
	// tag for "oci", optional docker-reference for "docker-archive"
	Arg2 string
}

func (r TransportRef) Format() (string, error) {
	return appendTransportRefTag(r.Transport, r.Arg1, r.Arg2)
}

// appendTransportRefTag appends ref to transport.
// See https://github.com/containers/skopeo/blob/main/docs/skopeo.1.md#image-names
func appendTransportRefTag(transport Transport, arg1, arg2 string) (string, error) {
	if arg1 == "" {
		return "", fmt.Errorf("empty ref: %q:%q:%q", transport, arg1, arg2)
	}
	switch transport {
	case TransportContainersStorage, TransportDir, TransportDockerDaemon:
		// containers-storage:docker-reference
		// dir:path
		// docker-daemon:docker-reference
		return string(transport) + ":" + arg1, nil
	case TransportDocker:
		// docker://docker-reference
		return string(transport) + "://" + arg1, nil
	case TransportDockerArchive:
		// docker-archive:path[:docker-reference]
		if arg2 != "" {
			return string(transport) + ":" + arg1 + ":" + arg2, nil
		}
		return string(transport) + ":" + arg1, nil
	case TransportOci:
		// oci:path:tag
		if arg2 == "" {
			return "", fmt.Errorf("empty tag: %q:%q:%q", transport, arg1, arg2)
		}
		return string(transport) + ":" + arg1 + ":" + arg2, nil
	default:
		return "", fmt.Errorf("unkonwn transport: %q:%q:%q", transport, arg1, arg2)
	}
}

// Skopeo is a typed wrapper over the skopeo CLI.
type Skopeo struct {
	Runner cli.Runner

	// CompressionFormat sets `--dest-compress-format <format>` on
	// every copy operation when non-empty. Recognized by skopeo:
	// "gzip", "zstd", "zstd:chunked".
	CompressionFormat string
	// CompressionLevel sets `--dest-compress-level <n>` on every copy
	// operation when non-zero. Range is format-specific; consult
	// skopeo and the underlying compressor for valid values.
	CompressionLevel int
}

// Version returns the trimmed `skopeo --version` output.
func (s *Skopeo) Version(ctx context.Context) (string, error) {
	out, err := s.Runner.Run(ctx, []string{"--version"})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// InspectRaw returns the raw manifest bytes for src via
// `skopeo inspect --raw <src>`.
func (s *Skopeo) InspectRaw(ctx context.Context, src TransportRef) ([]byte, error) {
	srcStr, err := src.Format()
	if err != nil {
		return nil, err
	}
	return s.Runner.Run(ctx, []string{
		"inspect", "--raw",
		srcStr,
	})
}

// InspectRawShared inspects an entry of a dumped oci: layout that
// uses a shared blob pool and returns the raw manifest bytes. Wraps
// `skopeo inspect --raw`. src.Transport must be [TransportOci].
func (s *Skopeo) InspectRawShared(ctx context.Context, src TransportRef, sharedBlobDir string) ([]byte, error) {
	if src.Transport != TransportOci {
		return nil, fmt.Errorf("skopeo: InspectRawShared requires src.Transport == %q, got %q", TransportOci, src.Transport)
	}
	srcStr, err := src.Format()
	if err != nil {
		return nil, err
	}
	return s.Runner.Run(ctx, []string{
		"inspect", "--raw",
		"--shared-blob-dir", sharedBlobDir,
		srcStr,
	})
}

// Copy copies src into dst using the shared blob pool at
// sharedBlobDir. Wraps `skopeo copy`. The shared-blob-dir flag is
// applied as `--src-shared-blob-dir` when src.Transport ==
// [TransportOci], or `--dest-shared-blob-dir` when dst.Transport ==
// [TransportOci]; passing a non-empty sharedBlobDir requires exactly
// one side to be OCI.
func (s *Skopeo) Copy(ctx context.Context, src, dst TransportRef, sharedBlobDir string) error {
	srcStr, err := src.Format()
	if err != nil {
		return err
	}
	dstStr, err := dst.Format()
	if err != nil {
		return err
	}

	if srcStr == dstStr {
		return fmt.Errorf("src and dst is same: %q", srcStr)
	}

	argv := []string{"copy"}
	argv = append(argv, s.compressionArgs()...)
	if sharedBlobDir != "" {
		switch {
		case src.Transport == TransportOci:
			argv = append(argv, "--src-shared-blob-dir", sharedBlobDir)
		case dst.Transport == TransportOci:
			argv = append(argv, "--dest-shared-blob-dir", sharedBlobDir)
		default:
			return fmt.Errorf("skopeo: sharedBlobDir requires one side to be %q (got src=%q dst=%q)",
				TransportOci, src.Transport, dst.Transport)
		}
	}
	argv = append(argv, srcStr, dstStr)
	_, err = s.Runner.Run(ctx, argv)
	return err
}

func (s *Skopeo) compressionArgs() []string {
	var args []string
	if s.CompressionFormat != "" {
		args = append(args, "--dest-compress-format", s.CompressionFormat)
	}
	if s.CompressionLevel != 0 {
		args = append(args, "--dest-compress-level", strconv.Itoa(s.CompressionLevel))
	}
	return args
}
