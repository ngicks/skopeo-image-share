package ocidir

import (
	"strings"
	"testing"
)

const ociManifestFixture = `{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": {
    "mediaType": "application/vnd.oci.image.config.v1+json",
    "digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
    "size": 1234
  },
  "layers": [
    {
      "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
      "digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "size": 5
    },
    {
      "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
      "digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
      "size": 6
    }
  ]
}`

const dockerV2ManifestFixture = `{
  "schemaVersion": 2,
  "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
  "config": {
    "mediaType": "application/vnd.docker.container.image.v1+json",
    "digest": "sha256:2222222222222222222222222222222222222222222222222222222222222222",
    "size": 999
  },
  "layers": [
    {
      "mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip",
      "digest": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
      "size": 7
    }
  ]
}`

const ociIndexFixture = `{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.index.v1+json",
  "manifests": [
    {
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "digest": "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
      "size": 4321,
      "platform": {"architecture": "amd64", "os": "linux"}
    }
  ]
}`

func TestParseManifest_OCI(t *testing.T) {
	t.Parallel()
	m, err := ParseManifest([]byte(ociManifestFixture))
	if err != nil {
		t.Fatal(err)
	}
	if string(m.Config.Digest) != "sha256:"+strings.Repeat("1", 64) {
		t.Errorf("config digest = %q", m.Config.Digest)
	}
	if got := LayerDigests(m); len(got) != 2 {
		t.Errorf("got %d layers, want 2", len(got))
	}
}

func TestParseManifest_Docker(t *testing.T) {
	t.Parallel()
	m, err := ParseManifest([]byte(dockerV2ManifestFixture))
	if err != nil {
		t.Fatal(err)
	}
	if string(m.Config.Digest) != "sha256:"+strings.Repeat("2", 64) {
		t.Errorf("config digest = %q", m.Config.Digest)
	}
	if got := LayerDigests(m); len(got) != 1 {
		t.Errorf("got %d layers, want 1", len(got))
	}
}

func TestParseManifest_RejectsIndex(t *testing.T) {
	t.Parallel()
	_, err := ParseManifest([]byte(ociIndexFixture))
	if err == nil {
		t.Fatal("expected error parsing OCI index as manifest")
	}
	if !strings.Contains(err.Error(), "index") && !strings.Contains(err.Error(), "list") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseManifest_MissingConfigDigest(t *testing.T) {
	t.Parallel()
	_, err := ParseManifest([]byte(`{"schemaVersion":2,"layers":[]}`))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "config.digest") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseIndex_OK(t *testing.T) {
	t.Parallel()
	idx, err := ParseIndex([]byte(ociIndexFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Manifests) != 1 {
		t.Fatalf("got %d manifests", len(idx.Manifests))
	}
	if string(idx.Manifests[0].Digest) != "sha256:"+strings.Repeat("d", 64) {
		t.Errorf("manifest digest = %q", idx.Manifests[0].Digest)
	}
}

func TestParseIndex_Empty(t *testing.T) {
	t.Parallel()
	_, err := ParseIndex([]byte(`{"schemaVersion":2,"manifests":[]}`))
	if err == nil {
		t.Fatal("expected error for empty manifests")
	}
}

func TestDigestBytes(t *testing.T) {
	t.Parallel()
	got := DigestBytes([]byte(""))
	want := "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != want {
		t.Errorf("DigestBytes(\"\") = %q, want %q", got, want)
	}
}
