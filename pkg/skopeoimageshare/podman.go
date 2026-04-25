package skopeoimageshare

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Podman is a typed wrapper over the podman CLI.
type Podman struct {
	Runner CommandRunner
}

// NewPodman returns a [Podman] driving r.
func NewPodman(r CommandRunner) *Podman { return &Podman{Runner: r} }

// Version returns the trimmed `podman --version` output.
func (p *Podman) Version(ctx context.Context) (string, error) {
	out, err := p.Runner.Run(ctx, []string{"--version"})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// podmanImage is the subset of `podman image ls --format json` that we
// need: per-image refs (Names) plus RepoDigests as a fallback for
// dangling images.
type podmanImage struct {
	Id          string   `json:"Id"`
	Names       []string `json:"Names"`
	RepoDigests []string `json:"RepoDigests"`
}

// ImageLs returns the union of all image refs visible to podman. Output
// of `podman image ls --format json` is a JSON array; refs come from
// each image's Names list (RepoTag-style).
func (p *Podman) ImageLs(ctx context.Context) ([]string, error) {
	out, err := p.Runner.Run(ctx, []string{"image", "ls", "--format", "json"})
	if err != nil {
		return nil, err
	}
	imgs, err := ParsePodmanImageLs(out)
	if err != nil {
		return nil, err
	}
	return imageRefsFromPodmanList(imgs), nil
}

// ParsePodmanImageLs parses `podman image ls --format json` output. It
// is exposed for unit tests using fixture data.
func ParsePodmanImageLs(out []byte) ([]podmanImage, error) {
	var imgs []podmanImage
	if err := json.Unmarshal(out, &imgs); err != nil {
		return nil, fmt.Errorf("podman: parse image ls json: %w", err)
	}
	return imgs, nil
}

func imageRefsFromPodmanList(imgs []podmanImage) []string {
	seen := map[string]struct{}{}
	var refs []string
	for _, img := range imgs {
		for _, n := range img.Names {
			if n == "" {
				continue
			}
			if _, dup := seen[n]; dup {
				continue
			}
			seen[n] = struct{}{}
			refs = append(refs, n)
		}
	}
	return refs
}
