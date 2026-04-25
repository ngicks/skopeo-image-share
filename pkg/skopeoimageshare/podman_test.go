package skopeoimageshare

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPodman_Version(t *testing.T) {
	writeShim(t, "podman", `echo "podman version 5.8.0"`)
	p := NewPodman(NewLocalRunner("podman"))
	v, err := p.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != "podman version 5.8.0" {
		t.Errorf("version = %q", v)
	}
}

func TestPodman_ImageLs_Fixture(t *testing.T) {
	wd, _ := os.Getwd()
	fixture, err := os.ReadFile(filepath.Join(wd, "..", "..", "testdata", "podman-image-ls-json.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	dir := t.TempDir()
	dataPath := filepath.Join(dir, "out.json")
	if err := os.WriteFile(dataPath, fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(dir, "podman")
	body := "#!/bin/sh\ncat " + dataPath + "\n"
	if err := os.WriteFile(scriptPath, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	p := NewPodman(NewLocalRunner("podman"))
	refs, err := p.ImageLs(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Sanity: fixture has many tagged refs (Names != null). Spot-check a
	// known one.
	wantOne := "docker.io/library/ubuntu:noble-20260410"
	if !slices.Contains(refs, wantOne) {
		t.Errorf("expected fixture to yield ref %q; got %d refs, e.g. %v",
			wantOne, len(refs), refs[:min(5, len(refs))])
	}
}

func TestParsePodmanImageLs_Empty(t *testing.T) {
	t.Parallel()
	imgs, err := ParsePodmanImageLs([]byte(`[]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 0 {
		t.Errorf("expected empty, got %v", imgs)
	}
	if got := imageRefsFromPodmanList(imgs); len(got) != 0 {
		t.Errorf("expected no refs, got %v", got)
	}
}

func TestParsePodmanImageLs_DeduplicatesNames(t *testing.T) {
	t.Parallel()
	json := `[
	  {"Id":"a","Names":["x:1","x:1","y:2"]},
	  {"Id":"b","Names":["x:1"]}
	]`
	imgs, err := ParsePodmanImageLs([]byte(json))
	if err != nil {
		t.Fatal(err)
	}
	got := imageRefsFromPodmanList(imgs)
	if len(got) != 2 {
		t.Errorf("expected 2 unique refs, got %v", got)
	}
}
