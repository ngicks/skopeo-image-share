// Package skopeo is a typed wrapper over the skopeo CLI. It does not
// look at flag spellings or the installed skopeo version; runtime
// errors surface via the [Runner] implementation.
package skopeo

import (
	"context"
	"strings"

	"github.com/ngicks/skopeo-image-share/pkg/cli"
)

// Skopeo is a typed wrapper over the skopeo CLI.
type Skopeo struct {
	Runner cli.Runner
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
//	    --dest-shared-blob-dir <sharedBlobDir> \
//	    <srcTransport>:<srcRef> oci:<ociDir>
func (s *Skopeo) CopyToOCI(ctx context.Context, srcTransport, srcRef, ociDir, sharedBlobDir string) error {
	_, err := s.Runner.Run(ctx, []string{
		"copy", "--preserve-digests",
		"--dest-shared-blob-dir", sharedBlobDir,
		srcTransport + ":" + srcRef,
		"oci:" + ociDir,
	})
	return err
}

// CopyFromOCI runs
//
//	skopeo copy --preserve-digests \
//	    --src-shared-blob-dir <sharedBlobDir> \
//	    oci:<ociDir> <dstTransport>:<dstRef>
func (s *Skopeo) CopyFromOCI(ctx context.Context, ociDir, sharedBlobDir, dstTransport, dstRef string) error {
	_, err := s.Runner.Run(ctx, []string{
		"copy", "--preserve-digests",
		"--src-shared-blob-dir", sharedBlobDir,
		"oci:" + ociDir,
		dstTransport + ":" + dstRef,
	})
	return err
}
