package skopeoimageshare

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDocker_Version(t *testing.T) {
	writeShim(t, "docker", `echo "Docker version 26.1.3, build abcdef"`)
	d := NewDocker(NewLocalRunner("docker"))
	v, err := d.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != "Docker version 26.1.3, build abcdef" {
		t.Errorf("version = %q", v)
	}
}

func TestDocker_ImageLs_Fixture(t *testing.T) {
	wd, _ := os.Getwd()
	fixture, err := os.ReadFile(filepath.Join(wd, "..", "..", "testdata", "docker-image-ls-json.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	dir := t.TempDir()
	dataPath := filepath.Join(dir, "out.json")
	if err := os.WriteFile(dataPath, fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(dir, "docker")
	body := "#!/bin/sh\ncat " + dataPath + "\n"
	if err := os.WriteFile(scriptPath, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	d := NewDocker(NewLocalRunner("docker"))
	refs, err := d.ImageLs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantOne := "ubuntu:noble-20260410"
	if !slices.Contains(refs, wantOne) {
		t.Errorf("expected fixture to yield ref %q; got %d refs, e.g. %v",
			wantOne, len(refs), refs[:min(5, len(refs))])
	}
}

func TestDocker_ImageLs_NDJSON(t *testing.T) {
	writeShim(t, "docker", `
cat <<'EOF'
{"ID":"sha256:0b","Repository":"ubuntu","Tag":"noble-20260410","Digest":"sha256:c4"}
{"ID":"sha256:0b","Repository":"<none>","Tag":"<none>","Digest":""}
{"ID":"sha256:50","Repository":"plantuml/plantuml-server","Tag":"jetty-v1.2026.2","Digest":""}
EOF
`)
	d := NewDocker(NewLocalRunner("docker"))
	refs, err := d.ImageLs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ubuntu:noble-20260410", "plantuml/plantuml-server:jetty-v1.2026.2"}
	if len(refs) != len(want) {
		t.Fatalf("refs count: got %d (%v), want %d", len(refs), refs, len(want))
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Errorf("refs[%d]: got %q, want %q", i, refs[i], want[i])
		}
	}
}

func TestParseDockerImageLs_Empty(t *testing.T) {
	t.Parallel()
	imgs, err := ParseDockerImageLs([]byte(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 0 {
		t.Errorf("expected empty, got %v", imgs)
	}
}

func TestParseDockerImageLs_Dedup(t *testing.T) {
	t.Parallel()
	in := `{"Repository":"x","Tag":"1"}
{"Repository":"x","Tag":"1"}
{"Repository":"y","Tag":"2"}`
	imgs, err := ParseDockerImageLs([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	got := imageRefsFromDockerList(imgs)
	if len(got) != 2 {
		t.Errorf("expected 2 deduped refs, got %v", got)
	}
}

// TestParseDockerImageInspect_Fixture verifies that the
// testdata/docker-image-inspect.json sample parses cleanly into
// RepoTags. The live enumeration path uses `image ls --format json`,
// not inspect; this test exists so the alternative parser (kept for
// future use) is verified against a real sample shape.
func TestParseDockerImageInspect_Fixture(t *testing.T) {
	t.Parallel()
	wd, _ := os.Getwd()
	fixture, err := os.ReadFile(filepath.Join(wd, "..", "..", "testdata", "docker-image-inspect.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	imgs, err := ParseDockerImageInspect(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) == 0 {
		t.Fatal("expected at least one image in fixture")
	}
	refs := imageRefsFromDockerInspect(imgs)
	wantOne := "ubuntu:noble-20260410"
	if !slices.Contains(refs, wantOne) {
		t.Errorf("expected ref %q in %v", wantOne, refs)
	}
	if imgs[0].Id == "" {
		t.Error("expected Id field populated")
	}
}
