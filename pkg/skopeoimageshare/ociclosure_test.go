package skopeoimageshare

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// buildOCIDump materializes an oci: layout in dumpDir + a shared blob
// dir with the manifest blob (and only the manifest blob).
func buildOCIDump(t *testing.T) (dumpDir, shareDir, manifestDigest string) {
	t.Helper()
	root := t.TempDir()
	dumpDir = filepath.Join(root, "_tags", "v1")
	shareDir = filepath.Join(root, "share")
	if err := os.MkdirAll(dumpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(shareDir, "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dumpDir, "index.json"), []byte(indexJSONFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dumpDir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestDigest = "sha256:" + strings.Repeat("d", 64)
	if err := os.WriteFile(filepath.Join(shareDir, "sha256", strings.Repeat("d", 64)),
		[]byte(ociManifestFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	return dumpDir, shareDir, manifestDigest
}

func TestOCIClosure_OCIManifest(t *testing.T) {
	t.Parallel()
	dumpDir, shareDir, manifestDigest := buildOCIDump(t)

	c, err := OCIClosure(LocalBlobReader{}, dumpDir, shareDir)
	if err != nil {
		t.Fatal(err)
	}
	if c.ManifestDigest != manifestDigest {
		t.Errorf("ManifestDigest = %q, want %q", c.ManifestDigest, manifestDigest)
	}
	if c.ConfigDigest != "sha256:"+strings.Repeat("1", 64) {
		t.Errorf("ConfigDigest = %q", c.ConfigDigest)
	}
	if len(c.LayerDigests) != 2 {
		t.Errorf("LayerDigests len = %d, want 2", len(c.LayerDigests))
	}
	all := c.AllDigests()
	if len(all) != 4 {
		t.Errorf("AllDigests size = %d, want 4 (manifest+config+2 layers)", len(all))
	}
}

func TestOCIClosure_DockerV2(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dumpDir := filepath.Join(root, "_tags", "v1")
	shareDir := filepath.Join(root, "share")
	if err := os.MkdirAll(dumpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(shareDir, "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	dockerIndex := strings.ReplaceAll(indexJSONFixture, strings.Repeat("d", 64), strings.Repeat("e", 64))
	if err := os.WriteFile(filepath.Join(dumpDir, "index.json"), []byte(dockerIndex), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shareDir, "sha256", strings.Repeat("e", 64)),
		[]byte(dockerV2ManifestFixture), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := OCIClosure(LocalBlobReader{}, dumpDir, shareDir)
	if err != nil {
		t.Fatal(err)
	}
	if c.ConfigDigest != "sha256:"+strings.Repeat("2", 64) {
		t.Errorf("ConfigDigest = %q", c.ConfigDigest)
	}
	if len(c.LayerDigests) != 1 {
		t.Errorf("LayerDigests = %v, want 1", c.LayerDigests)
	}
}

func TestOCIClosure_MissingManifestBlob(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dumpDir := filepath.Join(root, "_tags", "v1")
	shareDir := filepath.Join(root, "share")
	if err := os.MkdirAll(dumpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(shareDir, "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dumpDir, "index.json"), []byte(indexJSONFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	// (no manifest blob)
	_, err := OCIClosure(LocalBlobReader{}, dumpDir, shareDir)
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

func TestPosixBlobPath(t *testing.T) {
	t.Parallel()
	p, err := PosixBlobPath("/r/share", "sha256:"+strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if p != "/r/share/sha256/"+strings.Repeat("a", 64) {
		t.Errorf("got %q", p)
	}
}
