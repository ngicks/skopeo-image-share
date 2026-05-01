package ocidir

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/ngicks/go-fsys-helper/vroot/osfs"
)

const indexJSONFixture = `{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.index.v1+json",
  "manifests": [
    {
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "digest": "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
      "size": 4321
    }
  ]
}`

func mustFs(t *testing.T, root string) vroot.Fs {
	t.Helper()
	v, err := osfs.NewUnrooted(root)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestReadManifest_Local walks pkg/ocidir/testdata/, finds every dump
// dir (any directory containing an `index.json`), and verifies it
// round-trips through [SharedFsDir] + [ReadManifest]. The `_Local`
// suffix marks it for skipping in CI; populate testdata locally with
// the recipe in pkg/ocidir/prep_testdata.go.
func TestReadManifest_Local(t *testing.T) {
	t.Parallel()
	const root = "testdata"
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		t.Skip("no pkg/ocidir/testdata/ dir")
	}

	dumps, err := findDumpDirs(root)
	if err != nil {
		t.Fatalf("findDumpDirs: %v", err)
	}
	if len(dumps) == 0 {
		t.Skip("no OCI dumps under pkg/ocidir/testdata/; run `go generate ./pkg/ocidir/` to populate")
	}

	for _, dumpDir := range dumps {
		t.Run(dumpDir, func(t *testing.T) {
			dir := NewSharedFsDir(
				NewFsDir(mustFs(t, filepath.Join(root, filepath.FromSlash(dumpDir)))),
				mustFs(t, filepath.Join(root, "share")),
			)

			layout, err := dir.ImageLayout()
			if err != nil {
				t.Fatalf("ImageLayout: %v", err)
			}
			if layout.Version == "" {
				t.Errorf("ImageLayout.Version empty")
			}

			mDesc, man, err := ReadManifest(dir)
			if err != nil {
				t.Fatalf("ReadManifest: %v", err)
			}
			if mDesc.Digest == "" {
				t.Error("manifest descriptor has empty digest")
			}
			if man.Config.Digest == "" {
				t.Error("manifest config has empty digest")
			}
			if got := len(AllDescriptors(mDesc, man)); got < 2 {
				t.Errorf("AllDescriptors size = %d, want >= 2 (manifest+config)", got)
			}
		})
	}
}

// findDumpDirs walks root and returns the relative paths of every
// directory that contains an `index.json` file. The "share" subdir is
// skipped (it holds the blob pool, not dumps).
func findDumpDirs(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "share" || strings.HasPrefix(rel, "share"+string(filepath.Separator)) {
			return filepath.SkipDir
		}
		if _, err := os.Stat(filepath.Join(p, "index.json")); err == nil {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out, err
}

func TestReadManifest_MissingManifestBlob(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.json"), []byte(indexJSONFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// (no manifest blob)
	_, _, err := ReadManifest(NewFsDir(mustFs(t, root)))
	if !errors.Is(err, ErrMissingManifestBlob) {
		t.Fatalf("expected ErrMissingManifestBlob, got %v", err)
	}
}

func TestSplitDigest(t *testing.T) {
	t.Parallel()
	algo, hex, err := SplitDigest("sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if algo != "sha256" || hex != strings.Repeat("a", 64) {
		t.Errorf("got %q,%q", algo, hex)
	}

	if _, _, err := SplitDigest("oops"); err == nil {
		t.Error("expected error for malformed digest")
	}
	if _, _, err := SplitDigest(":x"); err == nil {
		t.Error("expected error for empty algo")
	}
}
