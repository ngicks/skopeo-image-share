// Package skopeo is a typed wrapper over the skopeo CLI. It does not
// look at flag spellings or the installed skopeo version; runtime
// errors surface via the [Runner] implementation.
package skopeo

import (
	"context"
	"strconv"
	"strings"

	"github.com/ngicks/skopeo-image-share/pkg/cli"
)

// Skopeo is a typed wrapper over the skopeo CLI.
type Skopeo struct {
	Runner cli.Runner

	// CompressionFormat sets `--compression-format <format>` on every
	// copy operation when non-empty. Recognized by skopeo: "gzip",
	// "zstd", "zstd:chunked".
	CompressionFormat string
	// CompressionLevel sets `--compression-level <n>` on every copy
	// operation when non-zero. Range is format-specific; consult
	// skopeo and the underlying compressor for valid values.
	CompressionLevel int
	// ForceCompression sets `--force-compression` on every copy
	// operation. Recompresses already-compressed layers using
	// CompressionFormat / CompressionLevel.
	ForceCompression bool
}

// New returns a [Skopeo] that drives r.
func New(r cli.Runner) *Skopeo { return &Skopeo{Runner: r} }

// Version returns the trimmed `skopeo --version` output.
func (s *Skopeo) Version(ctx context.Context) (string, error) {
	out, err := s.Runner.Run(ctx, []string{"--version"})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// InspectRaw returns the raw manifest bytes for transport:ref via
// `skopeo inspect --raw <transport>:<ref>`.
func (s *Skopeo) InspectRaw(ctx context.Context, transport, ref string) ([]byte, error) {
	return s.Runner.Run(ctx, []string{
		"inspect", "--raw",
		transport + ":" + ref,
	})
}

// InspectRawShared is InspectRaw with --shared-blob-dir for inspecting a
// dumped oci: layout that uses a shared blob pool.
func (s *Skopeo) InspectRawShared(ctx context.Context, ociDir, sharedBlobDir string) ([]byte, error) {
	return s.Runner.Run(ctx, []string{
		"inspect", "--raw",
		"--shared-blob-dir", sharedBlobDir,
		"oci:" + ociDir,
	})
}

// CopyToOCI runs
//
//	skopeo copy --preserve-digests \
//	    [compression flags] \
//	    --dest-shared-blob-dir <sharedBlobDir> \
//	    <srcTransport>:<srcRef> oci:<ociDir>
func (s *Skopeo) CopyToOCI(ctx context.Context, srcTransport, srcRef, ociDir, sharedBlobDir string) error {
	argv := []string{"copy", "--preserve-digests"}
	argv = append(argv, s.compressionArgs()...)
	argv = append(argv,
		"--dest-shared-blob-dir", sharedBlobDir,
		srcTransport+":"+srcRef,
		"oci:"+ociDir,
	)
	_, err := s.Runner.Run(ctx, argv)
	return err
}

// CopyFromOCI runs
//
//	skopeo copy --preserve-digests \
//	    [compression flags] \
//	    --src-shared-blob-dir <sharedBlobDir> \
//	    oci:<ociDir> <dstTransport>:<dstRef>
func (s *Skopeo) CopyFromOCI(ctx context.Context, ociDir, sharedBlobDir, dstTransport, dstRef string) error {
	argv := []string{"copy", "--preserve-digests"}
	argv = append(argv, s.compressionArgs()...)
	argv = append(argv,
		"--src-shared-blob-dir", sharedBlobDir,
		"oci:"+ociDir,
		dstTransport+":"+dstRef,
	)
	_, err := s.Runner.Run(ctx, argv)
	return err
}

// compressionArgs returns the `--compression-*` / `--force-compression`
// flags derived from the CompressionFormat / CompressionLevel /
// ForceCompression fields. Zero values omit their flags.
func (s *Skopeo) compressionArgs() []string {
	var args []string
	if s.CompressionFormat != "" {
		args = append(args, "--compression-format", s.CompressionFormat)
	}
	if s.CompressionLevel != 0 {
		args = append(args, "--compression-level", strconv.Itoa(s.CompressionLevel))
	}
	if s.ForceCompression {
		args = append(args, "--force-compression")
	}
	return args
}
