// Package skopeo is a typed wrapper over the skopeo CLI. It does not
// look at flag spellings or the installed skopeo version; runtime
// errors surface via the [Runner] implementation.
package skopeo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ngicks/skopeo-image-share/pkg/cli"
)

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

// InspectRaw returns the raw manifest bytes for transport:ref via
// `skopeo inspect --raw <transport>:<ref>`.
func (s *Skopeo) InspectRaw(ctx context.Context, transport, ref string) ([]byte, error) {
	return s.Runner.Run(ctx, []string{
		"inspect", "--raw",
		transport + ":" + ref,
	})
}

// InspectRawShared inspects an entry of a dumped oci: layout that
// uses a shared blob pool and returns the raw manifest bytes. Wraps
// `skopeo inspect --raw`. ociDir and imageRef are required.
func (s *Skopeo) InspectRawShared(ctx context.Context, ociDir, imageRef, sharedBlobDir string) ([]byte, error) {
	if ociDir == "" {
		return nil, errors.New("skopeo: empty ociDir")
	}
	if imageRef == "" {
		return nil, errors.New("skopeo: empty imageRef")
	}
	return s.Runner.Run(ctx, []string{
		"inspect", "--raw",
		"--shared-blob-dir", sharedBlobDir,
		"oci:" + ociDir + ":" + imageRef,
	})
}

// CopyToOCI copies <srcTransport>:<srcRef> into the oci: layout under
// ociDir using the shared blob pool at sharedBlobDir. Wraps
// `skopeo copy`. ociDir and imageRef are required; imageRef must match
// the value passed to a later [Skopeo.CopyFromOCI] reading the same dir.
func (s *Skopeo) CopyToOCI(ctx context.Context, srcTransport, srcRef, ociDir, imageRef, sharedBlobDir string) error {
	if ociDir == "" {
		return errors.New("skopeo: empty ociDir")
	}
	if imageRef == "" {
		return errors.New("skopeo: empty imageRef")
	}
	argv := []string{"copy"}
	argv = append(argv, s.compressionArgs()...)
	argv = append(argv,
		"--dest-shared-blob-dir", sharedBlobDir,
		srcTransport+":"+srcRef,
		"oci:"+ociDir+":"+imageRef,
	)
	_, err := s.Runner.Run(ctx, argv)
	return err
}

// CopyFromOCI copies an entry of the oci: layout under ociDir
// (selected by imageRef) into <dstTransport>:<dstRef> using the
// shared blob pool at sharedBlobDir. Wraps `skopeo copy`. ociDir and
// imageRef are required; imageRef must match the value used by the
// [Skopeo.CopyToOCI] that wrote the entry.
func (s *Skopeo) CopyFromOCI(ctx context.Context, ociDir, imageRef, sharedBlobDir, dstTransport, dstRef string) error {
	if ociDir == "" {
		return errors.New("skopeo: empty ociDir")
	}
	if imageRef == "" {
		return errors.New("skopeo: empty imageRef")
	}

	src, err := appendTransportRef("oci", ociDir, imageRef)
	if err != nil {
		return err
	}

	argv := []string{"copy"}
	argv = append(argv, s.compressionArgs()...)
	argv = append(argv,
		"--src-shared-blob-dir", sharedBlobDir,
		src,
		dstTransport+":"+dstRef,
	)

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

// appendTransportRef appends ref to transport.
// See https://github.com/containers/skopeo/blob/main/docs/skopeo.1.md#image-names
func appendTransportRef(transport, ref, tag string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("empty ref: %q:%q:%q", transport, ref, tag)
	}
	switch transport {
	case "containers-storage", "dir":
		// containers-storage:docker-reference
		// dir:path
		return transport + ":" + ref, nil
	case "docker":
		// docker://docker-reference
		return transport + "://" + ref, nil
	case "docker-archive":
		// docker-archive:path[:docker-reference]
		if tag != "" {
			return transport + ":" + ref + ":" + tag, nil
		}
		return transport + ":" + ref, nil
	case "oci":
		// oci:path:tag
		if tag == "" {
			return "", fmt.Errorf("empty tag: %q:%q:%q", transport, ref, tag)
		}
		return transport + ":" + ref + ":" + tag, nil
	default:
		return "", fmt.Errorf("unkonwn transport: %q:%q:%q", transport, ref, tag)
	}
}
