// Command dumpimages populates internal/testdata/ocidir/ with real
// OCI dumps so the orchestrator's `_Local` tests have something to
// chew on. It drives [pkg/cli/skopeo] (i.e. the system `skopeo`
// binary) to write the per-image entries into one shared blob pool
// at internal/testdata/ocidir/share + one OCI dir at
// internal/testdata/ocidir/image. Run from the repo root:
//
//	go run ./internal/cmd/dumpimages
//
// Sources use the `docker://` transport, so the dumper pulls each
// entry straight from its registry — no local pull (podman/docker)
// required. Add new entries to the `images` slice below.
package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ngicks/skopeo-image-share/pkg/cli"
	"github.com/ngicks/skopeo-image-share/pkg/cli/skopeo"
)

const (
	sharePath = "internal/testdata/ocidir/share"
	imagePath = "internal/testdata/ocidir/image"
	donePath  = "internal/testdata/ocidir/done"
)

// dumpSpec is one entry in the dump set. Src is the [skopeo.TransportRef]
// used as the source of `skopeo copy`; ImageRef is the index reference
// name written into image/index.json's
// `org.opencontainers.image.ref.name`.
type dumpSpec struct {
	Src skopeo.TransportRef
}

// images is the canonical dump set. Curated to give the `_Local`
// tests broad coverage:
//   - distroless/base-debian12 — multi-arch (catches the
//     `Manifests[0]` index-walking bug in [ocidir.ReadManifest]).
//   - distroless/static-debian12 — multi-arch, shares base layers
//     with the above (exercises share/ deduplication).
var images = []dumpSpec{
	{
		Src: skopeo.TransportRef{
			Transport: skopeo.TransportDocker,
			Arg1:      "gcr.io/distroless/base-debian12@sha256:9dce90e688a57e59ce473ff7bc4c80bc8fe52d2303b4d99b44f297310bbd2210",
		},
	},
	{
		Src: skopeo.TransportRef{
			Transport: skopeo.TransportDocker,
			Arg1:      "docker.io/library/memcached:1.6.41",
		},
	},
	{
		Src: skopeo.TransportRef{
			Transport: skopeo.TransportDocker,
			Arg1:      "docker.io/library/memcached@sha256:277e0c4f249b118e95ab10e535bae2fa1af772271d9152f3468e58d59348db56",
		},
	},
	{
		Src: skopeo.TransportRef{
			Transport: skopeo.TransportDocker,
			Arg1:      "docker.io/library/memcached:1.5.22",
		},
	},
	{
		Src: skopeo.TransportRef{
			Transport: skopeo.TransportDocker,
			Arg1:      "docker.io/library/memcached@sha256:12f7570b87465bdc8104c54389d6dbecff01270fd76b3853dd78a1937dd5d6c8",
		},
	},
}

func main() {
	flag.Parse()

	if _, err := os.Stat(donePath); err == nil {
		log.Printf("done flag file; if regeneration is needed, remove %q", donePath)
		return
	}

	if err := run(); err != nil {
		log.Fatal(err)
	}
	f, err := os.Create(donePath)
	if err == nil {
		_ = f.Close()
	}
}

func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := os.MkdirAll(sharePath, fs.ModePerm); err != nil {
		return fmt.Errorf("mkdir share: %w", err)
	}
	if err := os.MkdirAll(imagePath, fs.ModePerm); err != nil {
		return fmt.Errorf("mkdir image: %w", err)
	}

	sk := &skopeo.Skopeo{
		Runner:            cli.NewLocalRunner(),
		CompressionFormat: "zstd",
		CompressionLevel:  10,
	}

	for _, img := range images {
		srcStr, err := img.Src.Format()
		if err != nil {
			return fmt.Errorf("format %+v: %w", img.Src, err)
		}
		dst := skopeo.TransportRef{
			Transport: skopeo.TransportOci,
			Arg1: filepath.Join(
				filepath.FromSlash(imagePath),
				filepath.FromSlash(refStorePath(img.Src.Arg1)),
			),
			Arg2: img.Src.Arg1,
		}
		log.Printf("dumping %s -> %s:%s:%s", srcStr, dst.Transport, dst.Arg1, dst.Arg2)
		if err := os.MkdirAll(dst.Arg1, fs.ModePerm); err != nil {
			return fmt.Errorf("mkdir image: %w", err)
		}
		if err := sk.Copy(ctx, img.Src, dst, sharePath); err != nil {
			return fmt.Errorf("copy %s: %w", srcStr, err)
		}
	}
	log.Printf("dumped %d image(s) to oci:%s (share: %s)", len(images), imagePath, sharePath)
	return nil
}

func refStorePath(s string) string {
	if before, after, ok := strings.Cut(s, "@sha256:"); ok {
		return before + "/_digests/" + after
	}
	if before, after, ok := strings.Cut(s, ":"); ok {
		return before + "/_tags/" + after
	}
	panic(fmt.Errorf("unknown spec: %q", s))
}
