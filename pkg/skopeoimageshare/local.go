package skopeoimageshare

import (
	"context"
	"errors"
	"fmt"

	"github.com/ngicks/skopeo-image-share/pkg/cli"
	"github.com/ngicks/skopeo-image-share/pkg/cli/docker"
	"github.com/ngicks/skopeo-image-share/pkg/cli/skopeo"
)

// Local bundles the local-side dependencies the push/pull orchestrator
// consumes. It owns the resolved base data dir, the local skopeo /
// podman / docker wrappers, and an [FS] rooted at BaseDir. Build via
// [NewLocal]; emit [PullSide] / [PushSide] snapshots via the methods of
// the same name. The struct itself owns no goroutines or descriptors
// and does not require Close.
type Local struct {
	BaseDir   string
	Transport string
	OCIPath   string

	skopeoCli *skopeo.Skopeo
	lister    listInterface
	fs        FS
}

// LocalConfig configures [NewLocal].
//
//   - BaseDir is optional; an empty value falls back to [DefaultBaseDir].
//   - Transport is required: one of [TransportContainersStorage],
//     [TransportDockerDaemon], or [TransportOCI].
//   - OCIPath is required when Transport == [TransportOCI].
type LocalConfig struct {
	BaseDir   string
	Transport string
	OCIPath   string
}

// NewLocal resolves BaseDir, ensures the on-disk layout, and builds
// the local skopeo wrapper plus an [FS] rooted at BaseDir. A
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
	fs, err := NewLocalFS(base)
	if err != nil {
		return nil, err
	}
	l := &Local{
		BaseDir:   base,
		Transport: cfg.Transport,
		OCIPath:   cfg.OCIPath,
		skopeoCli: skopeo.New(cli.NewLocalRunner("skopeo")),
		fs:        fs,
	}
	switch cfg.Transport {
	case TransportContainersStorage:
		l.lister = docker.NewPodman(cli.NewLocalRunner("podman"))
	case TransportDockerDaemon:
		l.lister = docker.NewDocker(cli.NewLocalRunner("docker"))
	}
	return l, nil
}

// Skopeo returns the local skopeo wrapper.
func (l *Local) Skopeo() *skopeo.Skopeo { return l.skopeoCli }

// FS returns the local [FS] rooted at BaseDir.
func (l *Local) FS() FS { return l.fs }

// Dump runs `skopeo copy <Transport>:<ref> oci:<store-tag-dir>`,
// staging ref into the local store layout (per-tag dump dir + the
// shared blob pool under BaseDir/share). Returns the absolute tag
// directory.
func (l *Local) Dump(ctx context.Context, ref ImageRef) (string, error) {
	store := NewStore(l.BaseDir)
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
	if err := l.skopeoCli.CopyToOCI(ctx, l.Transport, ref.String(), tagDirAbs, store.ShareDir()); err != nil {
		return "", fmt.Errorf("dump: skopeo copy: %w", err)
	}
	return tagDirAbs, nil
}

// PullSide returns the snapshot consumed by [Pull].
func (l *Local) PullSide() PullSide {
	return PullSide{
		Skopeo:    l.skopeoCli,
		FS:        l.fs,
		BaseDir:   l.BaseDir,
		Transport: l.Transport,
		OCIPath:   l.OCIPath,
		Lister:    l.lister,
	}
}

// PushSide returns the snapshot consumed by [Push].
func (l *Local) PushSide() PushSide {
	return PushSide{
		Skopeo:    l.skopeoCli,
		FS:        l.fs,
		BaseDir:   l.BaseDir,
		Transport: l.Transport,
		OCIPath:   l.OCIPath,
	}
}
