package skopeoimageshare

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"

	"github.com/ngicks/go-common/contextkey"
)

// Transport names recognized by [Enumerate].
const (
	TransportContainersStorage = "containers-storage"
	TransportDockerDaemon      = "docker-daemon"
	TransportOCI               = "oci"
)

// SkopeoInspector is the [Skopeo] subset used by enumeration. The
// interface lets tests substitute a fake without spinning up a fake
// skopeo on $PATH.
type SkopeoInspector interface {
	InspectRaw(ctx context.Context, transport, ref string) ([]byte, error)
}

// PodmanLister is the [Podman] subset used by enumeration.
type PodmanLister interface {
	ImageLs(ctx context.Context) ([]string, error)
}

// DockerLister is the [Docker] subset used by enumeration.
type DockerLister interface {
	ImageLs(ctx context.Context) ([]string, error)
}

// EnumerateConfig is the bundle of inputs to [Enumerate].
type EnumerateConfig struct {
	// Transport selects the enumerator. One of [TransportContainersStorage],
	// [TransportDockerDaemon], or [TransportOCI].
	Transport string

	// Skopeo is required for containers-storage / docker-daemon to
	// turn refs into manifests.
	Skopeo SkopeoInspector

	// Podman is required when Transport == containers-storage.
	Podman PodmanLister

	// Docker is required when Transport == docker-daemon.
	Docker DockerLister

	// FS is the filesystem holding the share/ pool (for all three
	// transports) and, for OCI, the layout root rooted at BaseDir.
	FS FS

	// BaseDir is the application's data dir on this side
	// (`<base>` from PLAN §3). The enumerator unions
	// `<BaseDir>/share/sha256/*` filenames into the result.
	BaseDir string
}

// Enumerate dispatches to the right enumerator. The result is the
// union described by PLAN §4.2: `manifest digests` + `config digests`
// + `layer digests` reachable from peer refs, plus the filename set of
// `<peer-base>/share/`.
func Enumerate(ctx context.Context, cfg EnumerateConfig) (DigestSet, error) {
	switch cfg.Transport {
	case TransportContainersStorage:
		return enumerateViaSkopeoInspect(ctx, cfg, cfg.Podman, "containers-storage")
	case TransportDockerDaemon:
		return enumerateViaSkopeoInspect(ctx, cfg, cfg.Docker, "docker-daemon")
	case TransportOCI:
		return enumerateOCI(ctx, cfg)
	default:
		return nil, fmt.Errorf("enumerate: unsupported transport %q", cfg.Transport)
	}
}

// listInterface unifies PodmanLister and DockerLister.
type listInterface interface {
	ImageLs(ctx context.Context) ([]string, error)
}

func enumerateViaSkopeoInspect(ctx context.Context, cfg EnumerateConfig, lister listInterface, transport string) (DigestSet, error) {
	if lister == nil {
		return nil, fmt.Errorf("enumerate %s: missing lister", transport)
	}
	if cfg.Skopeo == nil {
		return nil, fmt.Errorf("enumerate %s: missing skopeo", transport)
	}

	logger := contextkey.ValueSlogLoggerDefault(ctx)
	out := NewDigestSet()

	refs, err := lister.ImageLs(ctx)
	if err != nil {
		return nil, fmt.Errorf("enumerate %s: list images: %w", transport, err)
	}

	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, err := cfg.Skopeo.InspectRaw(ctx, transport, ref)
		if err != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "enumerate.inspect.skip",
				slog.String("transport", transport),
				slog.String("ref", ref),
				slog.Any("err", err),
			)
			continue
		}
		man, err := ParseManifest(raw)
		if err != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "enumerate.parse.skip",
				slog.String("transport", transport),
				slog.String("ref", ref),
				slog.Any("err", err),
			)
			continue
		}
		out.Add(DigestBytes(raw))
		out.Add(man.Config.Digest)
		for _, l := range man.LayerDigests() {
			out.Add(l)
		}
	}

	if cfg.FS != nil {
		if err := unionShareInventory(out, cfg.FS, "share"); err != nil {
			return nil, fmt.Errorf("enumerate %s: share/ inventory: %w", transport, err)
		}
	}

	return out, nil
}

// enumerateOCI walks the on-disk layout under the FS's root, runs
// the closure walker on every tag/digest dump found, and unions the
// share pool's filename set.
func enumerateOCI(ctx context.Context, cfg EnumerateConfig) (DigestSet, error) {
	if cfg.FS == nil {
		return nil, errors.New("enumerate oci: nil FS")
	}

	out := NewDigestSet()

	dumps, err := walkDumpDirs(cfg.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("enumerate oci: walk: %w", err)
	}

	logger := contextkey.ValueSlogLoggerDefault(ctx)
	reader := fsBlobReader{fs: cfg.FS}
	for _, d := range dumps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c, err := OCIClosure(reader, d, "share")
		if err != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "enumerate.oci.closure.skip",
				slog.String("dump", d),
				slog.Any("err", err),
			)
			continue
		}
		for d := range c.AllDigests() {
			out.Add(d)
		}
	}

	if err := unionShareInventory(out, cfg.FS, "share"); err != nil {
		return nil, fmt.Errorf("enumerate oci: share/ inventory: %w", err)
	}
	return out, nil
}

// fsBlobReader adapts an [FS] to [BlobReader].
type fsBlobReader struct{ fs FS }

func (b fsBlobReader) ReadIndexJSON(dumpDir string) ([]byte, error) {
	return readAllVia(b.fs, path.Join(dumpDir, "index.json"))
}
func (b fsBlobReader) ReadBlob(shareDir, digest string) ([]byte, error) {
	algo, hex, err := SplitDigest(digest)
	if err != nil {
		return nil, err
	}
	data, err := readAllVia(b.fs, path.Join(shareDir, algo, hex))
	if err != nil {
		// translate fs not-exist to os.ErrNotExist so callers can use errors.Is
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	return data, nil
}

// walkDumpDirs walks the FS root listing immediate children, then
// for each non-reserved child recurses looking for `_tags`/`_digests`
// marker dirs. The returned paths are FS-relative leaf dump dirs.
func walkDumpDirs(fs FS, root string) ([]string, error) {
	hosts, err := readDirVia(fs, root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, fs2NotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, h := range hosts {
		if !h.IsDir() {
			continue
		}
		name := h.Name()
		if name == "share" || name == "tmp" || name == "log" {
			continue
		}
		var prefix string
		if root == "." || root == "" {
			prefix = name
		} else {
			prefix = path.Join(root, name)
		}
		if err := walkRepoTree(fs, prefix, &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// walkRepoTree recursively descends dir, treating any direct child
// named `_tags` or `_digests` as a marker dir whose own children are
// dump leaves.
func walkRepoTree(fs FS, dir string, out *[]string) error {
	entries, err := readDirVia(fs, dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		switch name {
		case "_tags", "_digests":
			markerDir := path.Join(dir, name)
			leaves, err := readDirVia(fs, markerDir)
			if err != nil {
				return err
			}
			for _, l := range leaves {
				if l.IsDir() {
					*out = append(*out, path.Join(markerDir, l.Name()))
				}
			}
		default:
			if err := walkRepoTree(fs, path.Join(dir, name), out); err != nil {
				return err
			}
		}
	}
	return nil
}

// fs2NotExist is a sentinel — io/fs.ErrNotExist. Aliased here so the
// errors.Is call above is readable inline.
var fs2NotExist = fs.ErrNotExist

// unionShareInventory adds every blob found under shareDir/sha256/ to
// dst. Missing share/ is treated as empty inventory.
func unionShareInventory(dst DigestSet, f FS, shareDir string) error {
	algoDir := path.Join(shareDir, "sha256")
	if _, ok, err := statSize(f, algoDir); err != nil {
		return err
	} else if !ok {
		return nil
	}
	entries, err := readDirVia(f, algoDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, fs2NotExist) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) != DigestHexLen {
			continue
		}
		dst.Add(DigestPrefix + name)
	}
	return nil
}

// DigestBytes returns the sha256 digest of b in `sha256:<hex>` form.
func DigestBytes(b []byte) string {
	h := sha256.Sum256(b)
	return DigestPrefix + hex.EncodeToString(h[:])
}
