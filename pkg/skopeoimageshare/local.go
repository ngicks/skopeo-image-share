package skopeoimageshare

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/ngicks/go-fsys-helper/vroot/osfs"
	"github.com/ngicks/skopeo-image-share/pkg/cli"
	"github.com/ngicks/skopeo-image-share/pkg/cli/docker"
	"github.com/ngicks/skopeo-image-share/pkg/cli/skopeo"
	"github.com/ngicks/skopeo-image-share/pkg/imageref"
)

// Local is the local-side push/pull endpoint. It owns the resolved
// base data dir, the local skopeo / podman / docker wrappers, and an
// [vroot.Fs] rooted at BaseDir. Build via [NewLocal].
//
// [Local.Push] and [Local.Pull] drive a transfer against any [Remote]
// (typically the SSH-backed implementation from [NewRemote]).
type Local struct {
	baseDir   string
	transport skopeo.Transport
	ociPath   string

	skopeoCli SkopeoLike
	lister    Lister
	fs        vroot.Fs

	validateOnce sync.Once
	validateErr  error
}

// LocalConfig configures [NewLocal].
//
//   - BaseDir is optional; an empty value falls back to [DefaultBaseDir].
//   - Transport is required: one of [skopeo.TransportContainersStorage],
//     [skopeo.TransportDockerDaemon], or [skopeo.TransportOci].
//   - OCIPath is required when Transport == [skopeo.TransportOci].
type LocalConfig struct {
	BaseDir   string
	Transport skopeo.Transport
	OCIPath   string
}

// NewLocal resolves BaseDir, ensures the on-disk layout, and builds
// the local skopeo wrapper plus an [vroot.Fs] rooted at BaseDir. A
// transport-appropriate lister (podman / docker) is wired up
// automatically.
func NewLocal(ctx context.Context, cfg LocalConfig) (*Local, error) {
	if cfg.Transport == "" {
		return nil, errors.New("local: transport unset")
	}
	base := cfg.BaseDir
	if base == "" {
		var err error
		base, err = DefaultBaseDir()
		if err != nil {
			return nil, err
		}
	}
	if err := NewStore(base).EnsureLayout(ctx); err != nil {
		return nil, fmt.Errorf("local: ensure layout: %w", err)
	}
	fs, err := osfs.NewUnrooted(base)
	if err != nil {
		return nil, fmt.Errorf("local: %w", err)
	}
	l := &Local{
		baseDir:   base,
		transport: cfg.Transport,
		ociPath:   cfg.OCIPath,
		skopeoCli: &skopeo.Skopeo{Runner: cli.NewLocalRunner()},
		fs:        fs,
	}
	switch cfg.Transport {
	case skopeo.TransportContainersStorage:
		l.lister = docker.NewPodman(cli.NewLocalRunner())
	case skopeo.TransportDockerDaemon:
		l.lister = docker.NewDocker(cli.NewLocalRunner())
	}
	return l, nil
}

// BaseDir returns the resolved local data dir.
func (l *Local) BaseDir() string { return l.baseDir }

// Transport returns the canonical local transport.
func (l *Local) Transport() skopeo.Transport { return l.transport }

// OCIPath returns the configured `oci:<dir>` path (only meaningful
// when Transport == [skopeo.TransportOci]).
func (l *Local) OCIPath() string { return l.ociPath }

// Skopeo returns the local skopeo wrapper.
func (l *Local) Skopeo() SkopeoLike { return l.skopeoCli }

// FS returns the local [vroot.Fs] rooted at BaseDir.
func (l *Local) FS() vroot.Fs { return l.fs }

// Lister returns the local docker / podman wrapper, or nil for
// [skopeo.TransportOci].
func (l *Local) Lister() Lister { return l.lister }

// Validate runs sanity checks against the local environment — at the
// moment, that the local skopeo binary is present and runnable.
// Cached after the first invocation; safe to call from every entry
// point.
func (l *Local) Validate(ctx context.Context) error {
	l.validateOnce.Do(func() {
		if _, err := l.skopeoCli.Version(ctx); err != nil {
			l.validateErr = fmt.Errorf("local skopeo: %w", err)
		}
	})
	return l.validateErr
}

// Dump runs `skopeo copy <Transport>:<ref> oci:<store-tag-dir>`,
// staging ref into the local store layout (per-tag dump dir + the
// shared blob pool under BaseDir/share). Returns the absolute tag
// directory.
func (l *Local) Dump(ctx context.Context, ref imageref.ImageRef) (string, error) {
	if err := l.Validate(ctx); err != nil {
		return "", err
	}
	store := NewStore(l.baseDir)
	tagDirAbs, err := store.DumpDir(ref)
	if err != nil {
		return "", err
	}
	tagDirRel, err := RelDumpDir(ref)
	if err != nil {
		return "", err
	}
	if err := l.fs.MkdirAll(tagDirRel, 0o755); err != nil {
		return "", fmt.Errorf("dump: mkdir %s: %w", tagDirRel, err)
	}
	if err := l.skopeoCli.Copy(ctx,
		skopeo.TransportRef{Transport: l.transport, Arg1: ref.String()},
		skopeo.TransportRef{Transport: skopeo.TransportOci, Arg1: tagDirAbs, Arg2: ref.String()},
		store.ShareDir(),
	); err != nil {
		return "", fmt.Errorf("dump: skopeo copy: %w", err)
	}
	return tagDirAbs, nil
}

// List returns the digest set of every blob reachable from this
// local's images, plus the share/ inventory.
func (l *Local) List(ctx context.Context) (map[string]struct{}, error) {
	return listAt(ctx, l.transport, l.skopeoCli, l.fs, l.baseDir, l.lister)
}
